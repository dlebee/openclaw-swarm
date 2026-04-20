//go:build integration_linode

package linode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// TestNodeSmoke is the Linode counterpart to the Multipass tier's
// TestNodeSmoke. It runs provisioning + security + mesh-gateway +
// mesh-join + gateway + node on a two-machine headscale-meshed
// manifest against real Linode hardware and asserts that the
// production pipeline — the same shape army.yml and david-army.yml
// drive in production — works end-to-end.
//
// Why this Linode mirror is worth the dollar-pennies cost on top of
// the Multipass tier's identical assertions:
//
//   - install-caddy + Let's Encrypt ACME. Multipass has no public IP
//     and runs the mesh with plain http://, so the Caddy/ACME path
//     is untested on that tier. On Linode the sslip strategy
//     resolves to a real public-routable hostname and Caddy actually
//     issues a cert against Let's Encrypt's production endpoint. A
//     regression in Caddy/ACME/headscale-over-HTTPS would surface
//     here first.
//   - headscale over real TLS. The control plane at
//     https://gateway.<dashed-ip>.sslip.io is the exact traffic
//     shape production tailscaled clients dial. Protocol-level
//     regressions (HTTP/2 handling, cert chain validation, ALPN)
//     show up here.
//   - The mesh → gateway → node pipeline over REAL internet hops
//     between two Linode VMs. Even once tailnet peers are
//     established, the openclaw-gateway → openclaw-node ws traffic
//     rides over the tailnet (WireGuard) which has real MTU and
//     timing characteristics Multipass's same-host bridge doesn't
//     expose.
//   - `npm install -g openclaw` on real Linode ubuntu24.04 — same
//     apt mirrors and npm registry egress as a customer install.
//
// Phase frontier: provisioning, security, mesh-gateway, mesh-join,
// gateway, node. Channels + agents aren't declared in the fixture.
// mesh.AddPhase splits mesh into two sub-phases when any gateway is
// headscale; OnlyPhases lists both explicitly.
//
// What this exercises over the Linode mesh + gateway tests
// COMBINED:
//
//   - TestMeshSmoke covers 3 VMs but stops at mesh-join (no gateway,
//     no node).
//   - TestGatewaySmoke covers 1 VM with gateway only, loopback bind,
//     no mesh.
//   - TestChannelsSmoke covers 1 VM with gateway + channels, loopback.
//
//   This test is the first that combines mesh + gateway + node, and
//   the only one that proves the node's openclaw-node daemon can
//   reach the gateway's openclaw-gateway daemon through the tailnet
//   on real cloud infrastructure. That's the single biggest value-
//   add of running it.
//
// Exec-policy: intentionally omitted from the fixture. ExecPolicyStep.
// Applicable=false when nt.Spec.ExecPolicy is nil, so the step is
// skipped — exactly the code path any user who doesn't declare a
// policy would hit. Agent tests will cover the Applicable=true
// branch.
//
// Cost envelope: ~15 min on two g6-standard-1 at us-east ≈ $0.008
// (each instance ~$0.015/hr, prorated over mesh-gateway ACME wait +
// gateway onboard + node install). 35min cap absorbs cold apt / npm
// registry / Let's Encrypt rate-limit backoff.
func TestNodeSmoke(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-node.yml")
	m.Prefix = "it-lin-node-" + randSuffix(t)
	prefix := m.Prefix

	if len(m.Machines) != 2 {
		t.Fatalf("fixture sanity: expected 2 machines, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	if len(m.Nodes) != 1 {
		t.Fatalf("fixture sanity: expected 1 node, got %d", len(m.Nodes))
	}
	gw := &m.Gateways[0]
	if gw.Name != "gateway" {
		t.Fatalf("fixture sanity: expected gateway name \"gateway\", got %q", gw.Name)
	}
	// headscale + sslip = the production shape. A drift to docker /
	// linode_vpc / custom would silently weaken the coverage (no
	// Caddy/ACME, no real-cloud mesh).
	if gw.Networking == nil || !strings.EqualFold(gw.Networking.Mode, "headscale") {
		t.Fatalf("fixture sanity: gateway %q networking.mode must be 'headscale' (this test is the production shape); got %+v",
			gw.Name, gw.Networking)
	}
	if gw.Networking.PublicHostname == nil ||
		!strings.EqualFold(gw.Networking.PublicHostname.Strategy, "sslip") {
		t.Fatalf("fixture sanity: gateway %q must set public_hostname.strategy=sslip (production default), got %+v",
			gw.Name, gw.Networking.PublicHostname)
	}
	if len(gw.Channels) != 0 {
		t.Fatalf("fixture sanity: gateway must have 0 channels (channels tier covers that), got %d",
			len(gw.Channels))
	}
	nd := &m.Nodes[0]
	if nd.Name != "worker-node" {
		t.Fatalf("fixture sanity: expected node name \"worker-node\", got %q", nd.Name)
	}
	if nd.Gateway != gw.Name {
		t.Fatalf("fixture sanity: node %q must reference gateway %q, got %q",
			nd.Name, gw.Name, nd.Gateway)
	}
	if nd.ExecPolicy != nil {
		t.Fatalf("fixture sanity: node must NOT declare exec_policy (covers the step-skipped code path); got %+v",
			nd.ExecPolicy)
	}
	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
		}
		// Same "agent must not be root" guard as every other
		// Linode test — flipping to root collapses the systemd
		// --user + linger coverage on both VMs simultaneously.
		if mc.AgentUser == "root" {
			t.Fatalf("fixture sanity: machine %q agent_user is 'root' — "+
				"TestNodeSmoke needs a dedicated agent account to "+
				"exercise the systemd --user + linger path", mc.Name)
		}
	}

	var gwMachine, nodeMachine manifestdata.Machine
	for _, mc := range m.Machines {
		switch mc.Name {
		case gw.Reference:
			gwMachine = mc
		case nd.Reference:
			nodeMachine = mc
		}
	}
	if gwMachine.Name == "" {
		t.Fatalf("fixture sanity: gateway reference %q matches no machine", gw.Reference)
	}
	if nodeMachine.Name == "" {
		t.Fatalf("fixture sanity: node reference %q matches no machine", nd.Reference)
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov := linode.NewProvider(tok)
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer ccancel()
		insts, err := prov.ListByTag(cleanupCtx, "claws/"+prefix)
		if err != nil {
			t.Logf("cleanup: list instances: %v", err)
			return
		}
		for _, inst := range insts {
			t.Logf("cleanup: deleting %s (%s)", inst.Label, inst.ResourceID)
			if err := prov.DeleteInstance(cleanupCtx, inst.ResourceID); err != nil {
				t.Logf("cleanup: delete %s: %v", inst.Label, err)
			}
		}
	})

	// --- plan ---------------------------------------------------------------

	plan, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:  m,
		Provider:  prov,
		SSHPubKey: pubKey,
		SSHDial:   dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	for _, want := range []string{"provisioning", "security", "mesh-gateway", "mesh-join", "gateway", "node"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + mesh + gateway + node on 2 Linode instances")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		// Explicit allowlist in dependency order.
		OnlyPhases: []string{"provisioning", "security", "mesh-gateway", "mesh-join", "gateway", "node"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- resolve public IPs post-apply -------------------------------------
	//
	// SSH into each VM uses the Linode PublicIPv4 — that's the
	// only address reachable from outside the tailnet. Tailscale
	// IPs are for the node-to-gateway dial INSIDE the test subject.

	gwMT := findMachineTarget(t, plan, gwMachine.Name)
	if gwMT.Instance == nil {
		t.Fatalf("gateway machine %q has no Instance after apply", gwMachine.Name)
	}
	gwInst := gwMT.Instance
	if gwInst.Status != "running" {
		t.Errorf("gateway machine %q status = %q, want %q", gwMachine.Name, gwInst.Status, "running")
	}
	if strings.TrimSpace(gwInst.PublicIPv4) == "" || net.ParseIP(gwInst.PublicIPv4) == nil {
		t.Fatalf("gateway machine %q PublicIPv4 %q is not a valid IP", gwMachine.Name, gwInst.PublicIPv4)
	}
	gwPublicIP := gwInst.PublicIPv4
	t.Logf("gateway VM → %s (public=%s)", gwInst.Label, gwPublicIP)

	nodeMT := findMachineTarget(t, plan, nodeMachine.Name)
	if nodeMT.Instance == nil {
		t.Fatalf("node machine %q has no Instance after apply", nodeMachine.Name)
	}
	nodeInst := nodeMT.Instance
	if nodeInst.Status != "running" {
		t.Errorf("node machine %q status = %q, want %q", nodeMachine.Name, nodeInst.Status, "running")
	}
	if strings.TrimSpace(nodeInst.PublicIPv4) == "" || net.ParseIP(nodeInst.PublicIPv4) == nil {
		t.Fatalf("node machine %q PublicIPv4 %q is not a valid IP", nodeMachine.Name, nodeInst.PublicIPv4)
	}
	nodePublicIP := nodeInst.PublicIPv4
	t.Logf("node VM → %s (public=%s)", nodeInst.Label, nodePublicIP)

	// --- tailnet IP discovery ----------------------------------------------
	//
	// Read each VM's tailnet address (100.64.x.y) via SSH over the
	// public IP. These are the addresses the node's openclaw-node.
	// service unit should reference via GatewayInternalHost's plan-
	// cache mesh-IP fallback. Proving that requires knowing the
	// expected value independently of whatever the plan cache
	// recorded.

	gwTailnetIP := readTailscaleIP(t, dial, gwPublicIP, gwMachine)
	if gwTailnetIP == "" {
		t.Fatalf("gateway VM %q did not join the tailnet (tailscale ip -4 empty)", gwMachine.Name)
	}
	if !isTailnetIP(gwTailnetIP) {
		t.Errorf("gateway VM %q tailscale IP %q is not in 100.64.0.0/10", gwMachine.Name, gwTailnetIP)
	}
	t.Logf("gateway VM %q tailnet=%s", gwMachine.Name, gwTailnetIP)

	nodeTailnetIP := readTailscaleIP(t, dial, nodePublicIP, nodeMachine)
	if nodeTailnetIP == "" {
		t.Fatalf("node VM %q did not join the tailnet (tailscale ip -4 empty)", nodeMachine.Name)
	}
	if !isTailnetIP(nodeTailnetIP) {
		t.Errorf("node VM %q tailscale IP %q is not in 100.64.0.0/10", nodeMachine.Name, nodeTailnetIP)
	}
	t.Logf("node VM %q tailnet=%s", nodeMachine.Name, nodeTailnetIP)

	if gwTailnetIP == nodeTailnetIP {
		t.Errorf("tailnet IP collision: both VMs got %s (headscale should allocate distinct addresses)", gwTailnetIP)
	}

	// --- gateway-side assertions -------------------------------------------

	assertNodeInstalled(t, dial, gwPublicIP, gwMachine)
	assertOpenclawInstalled(t, dial, gwPublicIP, gwMachine)
	assertGatewayUnitActive(t, dial, gwPublicIP, gwMachine)

	// Wildcard bind (0.0.0.0 / *:). The gateway-only / channels
	// Linode tests assert the inverse (MUST be loopback); this one
	// asserts the opposite branch because NeedsLANBind returns true
	// for headscale. The risk of this bind on a REAL public IP is
	// mitigated by UFW filtering from the security phase — only
	// tailnet peers and the local machine can hit :18789.
	assertGatewayPortListeningOnLAN(t, dial, gwPublicIP, gwMachine)

	assertOpenclawConfigExists(t, dial, gwPublicIP, gwMachine)
	assertOpenclawConfigValue(t, dial, gwPublicIP, gwMachine, "gateway.mode", "local")
	assertOpenclawConfigValue(t, dial, gwPublicIP, gwMachine, "gateway.bind", "lan")

	assertGatewayEnvDropIn(t, dial, gwPublicIP, gwMachine, "NODE_COMPILE_CACHE", "/var/tmp/openclaw-compile-cache")
	assertGatewayEnvDropIn(t, dial, gwPublicIP, gwMachine, "OPENCLAW_NO_RESPAWN", "1")
	assertGatewayEnvDropIn(t, dial, gwPublicIP, gwMachine, "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS", "1")

	// --- node-side assertions ----------------------------------------------

	assertNodeInstalled(t, dial, nodePublicIP, nodeMachine)
	assertOpenclawInstalled(t, dial, nodePublicIP, nodeMachine)

	assertNodeStubGatewayUnitPresent(t, dial, nodePublicIP, nodeMachine)
	assertNodeUnitActive(t, dial, nodePublicIP, nodeMachine)

	// THE single highest-signal assertion in this test: the
	// openclaw-node.service unit must reference the gateway's
	// TAILNET IP, not the public IP. This proves mesh-join
	// recorded the mesh IP into the plan cache and bootstrap-node's
	// GatewayInternalHost fallback chain picked it up over the
	// public host. See the Multipass tier's equivalent comment for
	// the full fallback-chain narrative.
	assertNodeUnitReferencesGateway(t, dial, nodePublicIP, nodeMachine, gwTailnetIP)

	// Negative check: the unit must NOT reference the gateway's
	// public IP. A unit that embeds both addresses would pass
	// the positive check above but still be wrong (the ws URL
	// would be built from whichever address came first) — this
	// guard closes that loophole.
	assertNodeUnitDoesNotReference(t, dial, nodePublicIP, nodeMachine, gwPublicIP)

	assertNodeEnvDropIn(t, dial, nodePublicIP, nodeMachine, "NODE_COMPILE_CACHE", "/var/tmp/openclaw-compile-cache")
	assertNodeEnvDropIn(t, dial, nodePublicIP, nodeMachine, "OPENCLAW_NO_RESPAWN", "1")
	assertNodeEnvDropIn(t, dial, nodePublicIP, nodeMachine, "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS", "1")

	// --- pairing cross-check -----------------------------------------------

	assertNodePairedOnGateway(t, dial, gwPublicIP, gwMachine, nd.Name)

	// --- teardown -----------------------------------------------------------

	insts, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after apply: %v", err)
	}
	if len(insts) != len(m.Machines) {
		t.Errorf("ListByTag returned %d instances, want %d", len(insts), len(m.Machines))
	}
	for _, inst := range insts {
		if err := prov.DeleteInstance(ctx, inst.ResourceID); err != nil {
			t.Errorf("DeleteInstance %s: %v", inst.Label, err)
		}
	}
	if after := waitForEmptyListByTag(ctx, t, prov, "claws/"+prefix, 45*time.Second); len(after) != 0 {
		t.Errorf("ListByTag after destroy still returned %d instances: %+v", len(after), after)
	}
}

// ---------------------------------------------------------------------------
// node-phase assertion helpers (Linode-local)
//
// Gateway-side helpers live in linode_gateway_test.go.
// Mesh/tailscale helpers (readTailscaleIP, isTailnetIP,
// sshRunAsAgent) live in linode_mesh_test.go. Everything below is
// node-specific, duplicated from the Multipass tier intentionally —
// build tags differ and the package-level helpers
// (loadLinodeToken, sshDialFunc, manifest loader) are Linode-
// specific. Cheap to review side-by-side.
// ---------------------------------------------------------------------------

// assertGatewayPortListeningOnLAN is the wildcard-bind sibling of
// linode_gateway_test.go's assertGatewayPortListening. For
// headscale mode the gateway MUST bind 0.0.0.0 (or [::]) —
// otherwise the node's tailnet peer can't reach :18789.
func assertGatewayPortListeningOnLAN(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastOut string
	for time.Now().Before(deadline) {
		out, _ := sshRunAsGatewayAgent(t, dial, host, mc,
			`ss -tln 2>/dev/null | awk '{print $4}' | grep -E ':18789$' || true`)
		if out != "" {
			t.Logf("[%s] :18789 listener: %s", mc.Name, out)
			isLAN := strings.HasPrefix(out, "0.0.0.0:") ||
				strings.HasPrefix(out, "*:") ||
				strings.HasPrefix(out, "[::]:")
			if !isLAN {
				t.Errorf("[%s] :18789 is listening but NOT on a wildcard address (bind=lan expected): %q",
					mc.Name, out)
			}
			return
		}
		lastOut = out
		time.Sleep(2 * time.Second)
	}
	t.Errorf("[%s] :18789 never started listening (last ss output: %q)", mc.Name, lastOut)
}

// sshRunAsNodeAgent is the node-side sibling of
// sshRunAsGatewayAgent. Same implementation; kept as a named helper
// so call sites stay self-documenting about which daemon's account
// we're dialing.
func sshRunAsNodeAgent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, cmd string) (string, error) {
	t.Helper()
	return sshRunAsGatewayAgent(t, dial, host, mc, cmd)
}

// assertNodeStubGatewayUnitPresent mirrors StubGatewayUnitStep.
// Verify's own check over SSH. If this passes but openclaw-node
// is missing, the bug moved downstream to bootstrap-node.
func assertNodeStubGatewayUnitPresent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsNodeAgent(t, dial, host, mc,
		`test -f "$HOME/.config/systemd/user/openclaw-gateway.service" && echo ok || echo missing`)
	if err != nil {
		t.Errorf("[%s] stat stub openclaw-gateway.service: %v\n%s", mc.Name, err, out)
		return
	}
	if out != "ok" {
		t.Errorf("[%s] stub openclaw-gateway.service missing under ~/.config/systemd/user", mc.Name)
	}
}

// assertNodeUnitActive is the openclaw-node twin of
// assertGatewayUnitActive. 30s retry envelope absorbs configure-
// node restart + cold TypeScript-compile lag.
func assertNodeUnitActive(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	cmd := `export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user is-active openclaw-node 2>&1`
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, _ := sshRunAsNodeAgent(t, dial, host, mc, cmd)
		if out == "active" {
			return
		}
		last = out
		time.Sleep(2 * time.Second)
	}
	t.Errorf("[%s] systemctl --user is-active openclaw-node never reached \"active\" (last: %q)",
		mc.Name, last)
}

// assertNodeUnitReferencesGateway greps the openclaw-node.service
// unit for the gateway host string. Highest-signal assertion in
// this test — see the Multipass tier's equivalent comment.
func assertNodeUnitReferencesGateway(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gwHost string) {
	t.Helper()
	out, err := sshRunAsNodeAgent(t, dial, host, mc,
		`cat "$HOME/.config/systemd/user/openclaw-node.service" 2>/dev/null || echo "(not found)"`)
	if err != nil {
		t.Errorf("[%s] read openclaw-node.service: %v\n%s", mc.Name, err, out)
		return
	}
	if strings.Contains(out, "(not found)") {
		t.Errorf("[%s] openclaw-node.service missing (bootstrap-node did not create the unit)", mc.Name)
		return
	}
	if !strings.Contains(out, gwHost) {
		t.Errorf("[%s] openclaw-node.service does not reference gateway host %q\nunit file:\n%s",
			mc.Name, gwHost, out)
	}
}

// assertNodeUnitDoesNotReference is the negative counterpart.
// Confirms the unit does NOT embed a forbidden host (e.g. the
// gateway's public IP when we expect the tailnet IP).
func assertNodeUnitDoesNotReference(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, forbidden string) {
	t.Helper()
	out, err := sshRunAsNodeAgent(t, dial, host, mc,
		`cat "$HOME/.config/systemd/user/openclaw-node.service" 2>/dev/null || echo "(not found)"`)
	if err != nil {
		t.Errorf("[%s] read openclaw-node.service: %v\n%s", mc.Name, err, out)
		return
	}
	if strings.Contains(out, "(not found)") {
		return // upstream assertion already failed
	}
	if strings.Contains(out, forbidden) {
		t.Errorf("[%s] openclaw-node.service must NOT reference %q (this would indicate the fallback chain picked the wrong host):\n%s",
			mc.Name, forbidden, out)
	}
}

// assertNodeEnvDropIn is the openclaw-node twin of
// assertGatewayEnvDropIn.
func assertNodeEnvDropIn(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, key, wantVal string) {
	t.Helper()
	cmd := `grep -H '^Environment=' "$HOME/.config/systemd/user/openclaw-node.service.d/"*.conf 2>/dev/null || ` +
		`grep -H '^Environment=' /etc/systemd/user/openclaw-node.service.d/*.conf 2>/dev/null || ` +
		`echo missing`
	out, err := sshRunAsNodeAgent(t, dial, host, mc, cmd)
	if err != nil {
		t.Errorf("[%s] read node env drop-in: %v\n%s", mc.Name, err, out)
		return
	}
	want := fmt.Sprintf(`%s=%s`, key, wantVal)
	if !strings.Contains(out, want) {
		t.Errorf("[%s] node env drop-in missing %q; got:\n%s", mc.Name, want, out)
	}
}

// assertNodePairedOnGateway SSHs to the GATEWAY and checks
// `openclaw devices list --json` for the node's displayName under
// paired, with zero pending.
func assertNodePairedOnGateway(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, nodeName string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastOut string
	for time.Now().Before(deadline) {
		out, err := sshRunAsGatewayAgent(t, dial, host, mc,
			`openclaw devices list --json 2>/dev/null`)
		if err != nil {
			lastOut = out
			time.Sleep(2 * time.Second)
			continue
		}
		lastOut = out
		var dl struct {
			Pending []struct {
				DisplayName string `json:"displayName"`
			} `json:"pending"`
			Paired []struct {
				DisplayName string `json:"displayName"`
				ClientMode  string `json:"clientMode"`
			} `json:"paired"`
		}
		if err := json.Unmarshal([]byte(out), &dl); err != nil {
			lastOut = out
			time.Sleep(2 * time.Second)
			continue
		}
		for _, p := range dl.Paired {
			if p.DisplayName == nodeName {
				t.Logf("[%s] gateway devices: node %q paired (mode=%s); pending=%d, paired=%d",
					mc.Name, nodeName, p.ClientMode, len(dl.Pending), len(dl.Paired))
				if len(dl.Pending) != 0 {
					t.Errorf("[%s] expected 0 pending devices after pair-node, got %d (raw: %s)",
						mc.Name, len(dl.Pending), out)
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("[%s] node %q never appeared in gateway devices list paired array (last raw: %s)",
		mc.Name, nodeName, lastOut)
}
