//go:build integration_multipass

package multipass

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// TestNodeSmoke is the sixth Multipass integration test: it runs
// provisioning + security + mesh-gateway + mesh-join + gateway + node
// on a two-machine, one-headscale-gateway, one-node
// manifest-node.yml fixture and asserts, over SSH against BOTH VMs,
// that the node phase landed the openclaw-node daemon, that the
// daemon is pointed at the gateway's TAILNET IP (not the Multipass
// bridge IP), and that pair-node approved the worker-node device on
// the gateway.
//
// This is the first Multipass test that combines mesh + gateway +
// node in a single apply — the production shape. Earlier tests cover
// slices in isolation:
//
//   - TestMeshSmoke runs mesh only (3 VMs, no gateway/node phases).
//   - TestGatewaySmoke runs gateway only on 1 VM, loopback bind.
//   - TestChannelsSmoke runs gateway + channels on 1 VM, loopback.
//
// None of them exercise the combined path where the node's
// openclaw-node daemon has to dial the gateway through the tailnet.
// That path is the one production operators (army.yml,
// david-army.yml) actually use, so this test is the first to prove
// it works end-to-end on a real tailnet wire.
//
// Phase frontier: provisioning, security, mesh-gateway, mesh-join,
// gateway, node. mesh.AddPhase splits mesh into two sub-phases when
// any gateway is headscale (mesh-gateway installs headscale on the
// gateway machine; mesh-join joins every participating VM — gateway
// included — to the tailnet). Channels + agents are not declared in
// the fixture and would be filtered out by OnlyPhases regardless.
//
// What this exercises that NO earlier test does:
//
//   - The combined mesh → gateway → node pipeline. mesh-join records
//     each VM's tailnet IP into the plan cache via
//     scaffold.RecordPlanMachineMeshIP. The gateway phase then starts
//     openclaw-gateway bound 0.0.0.0:18789 (NeedsLANBind is true for
//     headscale). The node phase's bootstrap-node then resolves the
//     gateway's internal host via NodeTarget.GatewayInternalHost,
//     which prefers the plan-cache mesh IP (step 2 in the fallback
//     chain) over the VM's Multipass bridge IP (step 4). Result: the
//     node's openclaw-node.service unit references 100.64.x.y, not
//     192.168.x.y.
//   - NeedsInsecureWS on both ends. The gateway's systemd env drop-in
//     gets OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 (otherwise the daemon
//     rejects plaintext ws:// from non-loopback clients). The node's
//     configure-node mirrors the same flag onto the node (otherwise
//     the openclaw-node client refuses to dial a non-loopback host
//     with "SECURITY ERROR: Cannot connect over plaintext"). This
//     test is the first to require BOTH flags to be present
//     simultaneously on different VMs.
//   - node.StubGatewayUnitStep on a meshed node. The stub unit is
//     the workaround for upstream bug 04 (openclaw node install
//     always enables openclaw-gateway, even on node-only hosts).
//     Applied here on a tailscale-enabled VM for the first time.
//   - node.PairNodeStep over a real tailnet. pair-node's "trigger
//     pending device entry via openclaw agents list, approve from
//     the gateway" dance runs end-to-end over the mesh — the ws
//     connection the node opens back to the gateway to register the
//     pending device is itself routed through 100.64.x.y.
//
// Exec-policy coverage: intentionally omitted from the fixture.
// ExecPolicyStep.Applicable=false when nt.Spec.ExecPolicy is nil, so
// the step is skipped by the scaffold executor — the exact code
// path any user who doesn't declare a policy would hit. Agent-layer
// tests will cover the Applicable=true branch when they're added.
//
// Runtime budget: provisioning ~60s × 2 concurrent VMs + security
// ~35s × 2 + mesh-gateway ~60s (install-headscale, gateway joins
// own tailnet) + mesh-join ~30s (node joins) + gateway ~5 min
// (NodeSource, apt, npm, onboard, restart, pair) + node ~4 min
// (NodeSource, apt, npm, stub, install, configure, pair). ~13 min
// realistic; 30 min cap absorbs cold apt/npm caches.
func TestNodeSmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-node.yml")
	m.Prefix = "it-node-" + randSuffix(t)
	prefix := m.Prefix
	// Rewrite the fixture's control URL now that we know the final
	// prefix — cloud-init will pin the VM's hostname to
	// `<prefix>-gateway-host` (see provisioning.hostedHostname), so
	// the manifest must advertise the same name or install-tailscale's
	// mDNS resolve would miss.
	rewriteMeshHost(m)

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
	// headscale is what makes this the "production shape" test.
	// NeedsLANBind + NeedsInsecureWS both flip for headscale (same
	// as docker / linode_vpc) but only headscale actually triggers
	// BuildMeshTargets to emit install-headscale + install-
	// tailscale steps — which is the whole point of using mesh
	// here instead of the docker shortcut.
	if gw.Networking == nil || !strings.EqualFold(gw.Networking.Mode, "headscale") {
		t.Fatalf("fixture sanity: gateway %q networking.mode must be 'headscale' (this test exercises the production mesh path); got %+v",
			gw.Name, gw.Networking)
	}
	// mDNS control URL: every Multipass VM advertises its machine
	// name via avahi, so `gateway-host.local` resolves from any
	// peer without a runtime IP-discovery step. If this assertion
	// fails because someone switched to sslip, the test would need
	// a public IP (Multipass doesn't have one) and would just hang
	// waiting for a Let's Encrypt cert.
	if gw.Networking.PublicHostname == nil ||
		!strings.EqualFold(gw.Networking.PublicHostname.Strategy, "custom") ||
		!strings.Contains(gw.Networking.PublicHostname.Host, ".local") {
		t.Fatalf("fixture sanity: gateway %q must set public_hostname.strategy=custom with a .local host, got %+v",
			gw.Name, gw.Networking.PublicHostname)
	}
	if len(gw.Channels) != 0 {
		t.Fatalf("fixture sanity: gateway must have 0 channels (TestChannelsSmoke covers channels), got %d",
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
	// No exec_policy on the node — this test covers the
	// ExecPolicyStep.Applicable=false skip path. If a future
	// refactor adds a default policy, move this guard to the
	// agent test.
	if nd.ExecPolicy != nil {
		t.Fatalf("fixture sanity: node must NOT declare exec_policy (covers the step-skipped code path); got %+v",
			nd.ExecPolicy)
	}
	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeMultipass {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
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

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		// Debug hook: if CLAWS_IT_KEEP_VMS is set, skip the
		// cleanup sweep so a failing test leaves the VMs running
		// for manual SSH inspection. Never set this in CI.
		if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
			t.Logf("CLAWS_IT_KEEP_VMS set → leaving VMs up for debug (prefix=%s)", prefix)
			return
		}
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer ccancel()
		insts, err := prov.ListByTag(cleanupCtx, "claws/"+prefix)
		if err != nil {
			t.Logf("cleanup: list instances: %v", err)
			return
		}
		for _, inst := range insts {
			t.Logf("cleanup: deleting %s", inst.Label)
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

	// mesh.AddPhase splits mesh into two sub-phases when any
	// gateway is headscale. If someone regresses mesh.AddPhase
	// into a single "mesh" phase, our OnlyPhases filter below
	// would silently drop the whole phase and the test would
	// pass-but-not-test-mesh.
	for _, want := range []string{"provisioning", "security", "mesh-gateway", "mesh-join", "gateway", "node"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + mesh + gateway + node on 2 VMs")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		// Explicit allowlist in dependency order. mesh-gateway
		// must run before mesh-join (install-headscale on the
		// control node); mesh-join must run before gateway
		// (gateway's bind=lan only makes sense on a tailnet-
		// joined VM); gateway must run before node (bootstrap-
		// node needs the gateway's auth token).
		OnlyPhases: []string{"provisioning", "security", "mesh-gateway", "mesh-join", "gateway", "node"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- resolve bridge IPs post-apply -------------------------------------
	//
	// SSH into each VM as agent user uses the Multipass bridge IP
	// (192.168.x.y) — that's what mt.Instance.PublicIPv4 reports.
	// Tailscale IPs are for the node-to-gateway dial INSIDE the
	// test subject; we reach both VMs from the host machine over
	// the bridge.

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
	gwBridgeIP := gwInst.PublicIPv4
	t.Logf("gateway VM → %s (bridge=%s)", gwInst.Label, gwBridgeIP)

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
	nodeBridgeIP := nodeInst.PublicIPv4
	t.Logf("node VM → %s (bridge=%s)", nodeInst.Label, nodeBridgeIP)

	// --- tailnet IP discovery ----------------------------------------------
	//
	// Read each VM's tailnet address by running `tailscale ip -4`
	// via SSH over the bridge. These are the addresses the node's
	// openclaw-node.service unit should reference (via
	// GatewayInternalHost's plan-cache mesh-IP fallback) — proving
	// that requires knowing the expected value independently of
	// whatever the plan cache recorded.

	gwTailnetIP := readTailscaleIP(t, dial, gwBridgeIP, gwMachine)
	if gwTailnetIP == "" {
		t.Fatalf("gateway VM %q did not join the tailnet (tailscale ip -4 empty)", gwMachine.Name)
	}
	if !isTailnetIP(gwTailnetIP) {
		t.Errorf("gateway VM %q tailscale IP %q is not in 100.64.0.0/10", gwMachine.Name, gwTailnetIP)
	}
	t.Logf("gateway VM %q tailnet=%s", gwMachine.Name, gwTailnetIP)

	nodeTailnetIP := readTailscaleIP(t, dial, nodeBridgeIP, nodeMachine)
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
	//
	// Re-use helpers from multipass_gateway_test.go (unit active,
	// openclaw installed, env drop-in NODE_COMPILE_CACHE /
	// OPENCLAW_NO_RESPAWN, config file exists). Divergence from
	// the gateway-only test: bind=lan (wildcard listener) and
	// OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 in the drop-in.

	assertNodeInstalled(t, dial, gwBridgeIP, gwMachine)
	assertOpenclawInstalled(t, dial, gwBridgeIP, gwMachine)
	assertGatewayUnitActive(t, dial, gwBridgeIP, gwMachine)

	// Wildcard bind: 0.0.0.0 (or [::] or *) not loopback —
	// headscale flips NeedsLANBind. Custom helper (the gateway-
	// only test's assertGatewayPortListening asserts the inverse
	// "must be loopback" invariant).
	assertGatewayPortListeningOnLAN(t, dial, gwBridgeIP, gwMachine)

	assertOpenclawConfigExists(t, dial, gwBridgeIP, gwMachine)
	assertOpenclawConfigValue(t, dial, gwBridgeIP, gwMachine, "gateway.mode", "local")
	// bind=lan because NeedsLANBind returns true for headscale.
	// The gateway-only / channels tests assert "loopback" here;
	// this test asserts the opposite branch.
	assertOpenclawConfigValue(t, dial, gwBridgeIP, gwMachine, "gateway.bind", "lan")

	assertGatewayEnvDropIn(t, dial, gwBridgeIP, gwMachine, "NODE_COMPILE_CACHE", "/var/tmp/openclaw-compile-cache")
	assertGatewayEnvDropIn(t, dial, gwBridgeIP, gwMachine, "OPENCLAW_NO_RESPAWN", "1")
	// If this flag is missing the gateway would reject the node's
	// plaintext ws:// connection during pair-node, and the test
	// would fail further down at assertNodePairedOnGateway — this
	// assertion pins the failure to the config layer first.
	assertGatewayEnvDropIn(t, dial, gwBridgeIP, gwMachine, "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS", "1")

	// --- node-side assertions ----------------------------------------------

	assertNodeInstalled(t, dial, nodeBridgeIP, nodeMachine)
	assertOpenclawInstalled(t, dial, nodeBridgeIP, nodeMachine)

	// Stub openclaw-gateway.service on the node host — workaround
	// for upstream bug 04. Without it, bootstrap-node's
	// `openclaw node install` fails at the `systemctl --user
	// enable openclaw-gateway` step and node.json never lands.
	assertNodeStubGatewayUnitPresent(t, dial, nodeBridgeIP, nodeMachine)

	assertNodeUnitActive(t, dial, nodeBridgeIP, nodeMachine)

	// THE single highest-signal assertion in this test: the
	// openclaw-node.service unit on node-host must reference the
	// gateway's TAILNET IP, not the bridge IP. This proves:
	//
	//   1. mesh-join ran install-tailscale on the gateway VM and
	//      recorded 100.64.x.y into the plan cache via
	//      scaffold.RecordPlanMachineMeshIP (install_tailscale.go:45).
	//   2. node.BootstrapNodeStep called NodeTarget.
	//      GatewayInternalHost, which pulled that mesh IP out of
	//      the plan cache (node.go:53 — step 2 in the fallback
	//      chain, taking precedence over both manifest Host and
	//      plan-cache public host).
	//   3. `openclaw node install --host <gateway-tailnet-ip>` ran
	//      and wrote that host into the systemd unit.
	//
	// A regression in ANY of those three steps would show up here
	// as "unit references bridge IP, want tailnet IP" — a much
	// clearer signal than the downstream pair-node timeout that
	// would otherwise hide the root cause.
	assertNodeUnitReferencesGateway(t, dial, nodeBridgeIP, nodeMachine, gwTailnetIP)

	// Also assert the unit does NOT reference the bridge IP — if
	// both IPs appear, the fallback chain's precedence order is
	// broken and we're connecting via the wrong route.
	assertNodeUnitDoesNotReference(t, dial, nodeBridgeIP, nodeMachine, gwBridgeIP)

	assertNodeEnvDropIn(t, dial, nodeBridgeIP, nodeMachine, "NODE_COMPILE_CACHE", "/var/tmp/openclaw-compile-cache")
	assertNodeEnvDropIn(t, dial, nodeBridgeIP, nodeMachine, "OPENCLAW_NO_RESPAWN", "1")
	assertNodeEnvDropIn(t, dial, nodeBridgeIP, nodeMachine, "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS", "1")

	// --- pairing cross-check -----------------------------------------------
	//
	// The gateway is where pairing state actually lives, so the
	// authoritative check is `openclaw devices list --json` on the
	// gateway VM. worker-node's displayName (== nd.Spec.Name) must
	// appear in the paired array, and pending must be empty.

	assertNodePairedOnGateway(t, dial, gwBridgeIP, gwMachine, nd.Name)

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
}

// ---------------------------------------------------------------------------
// node-phase assertion helpers
//
// Gateway-side helpers live in multipass_gateway_test.go and are
// reused above without change. Mesh-side helpers (readTailscaleIP,
// isTailnetIP, sshRunAsAgent) live in multipass_mesh_test.go and are
// reused for the tailnet-IP discovery above. Everything below is
// node-specific.
// ---------------------------------------------------------------------------

// assertGatewayPortListeningOnLAN is the LAN-bind sibling of
// assertGatewayPortListening (which insists on loopback). A gateway
// with mode=headscale (or docker / linode_vpc) must bind 0.0.0.0:
// 18789 so peers on the tailnet or the bridge can reach it.
func assertGatewayPortListeningOnLAN(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	// 30s retry envelope matches the loopback variant — the
	// gateway unit was just restarted by configure-gateway and the
	// listener can lag the "active" report by a few seconds.
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
// sshRunAsGatewayAgent — same implementation, kept separate so call
// sites read naturally ("ssh as node agent" vs. "ssh as gateway
// agent") even when the dial mechanics are identical.
func sshRunAsNodeAgent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, cmd string) (string, error) {
	t.Helper()
	return sshRunAsGatewayAgent(t, dial, host, mc, cmd)
}

// assertNodeStubGatewayUnitPresent mirrors StubGatewayUnitStep.
// Verify's own check over SSH. If this passes but openclaw-node is
// missing, the bug moved downstream to bootstrap-node; if it fails
// standalone, the stub step regressed.
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
// assertGatewayUnitActive. 30s retry envelope absorbs the
// configure-node restart + cold TypeScript-compile lag that can
// leave is-active reporting "activating" for a few seconds after
// the step returns success.
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
// unit file for the gateway host string. The highest-signal
// assertion in this test: a mismatch means bootstrap-node resolved
// the gateway host differently from what install-tailscale recorded
// in the plan cache, which would silently dial the wrong address
// and pair-node would timeout. Running this BEFORE the pairing
// cross-check pins the regression to GatewayInternalHost
// resolution specifically.
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

// assertNodeUnitDoesNotReference is the negative counterpart:
// confirm the unit does NOT embed a host we DON'T want it to (e.g.
// the bridge IP when we expect the tailnet IP). A unit that
// references BOTH the tailnet IP and the bridge IP — say, because
// GatewayInternalHost's fallback chain built a concatenated URL —
// would pass assertNodeUnitReferencesGateway but still be wrong.
// This guard closes that loophole.
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
// assertGatewayEnvDropIn. Same drop-in path
// (~agent/.config/systemd/user/<unit>.service.d/), different unit
// name. Kept as a dedicated helper so call sites stay self-
// documenting about which daemon's config matters.
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

// assertNodePairedOnGateway SSHs to the GATEWAY (not the node) and
// runs `openclaw devices list --json`. Pair-node approves the
// pending entry for the node's display name (== node.Spec.Name)
// and then restarts openclaw-node; once that restart completes the
// node reconnects and appears under `paired`. The DisplayName match
// is what distinguishes the node from the gateway's own CLI device
// (which also paired via pair-gateway-device during the gateway
// phase).
func assertNodePairedOnGateway(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, nodeName string) {
	t.Helper()
	// Short retry envelope: pair-node's Execute already polls
	// until approve returned success, but the node's post-restart
	// reconnect is asynchronous and can lag by a few seconds.
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
