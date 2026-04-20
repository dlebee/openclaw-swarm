//go:build integration_multipass

package multipass

import (
	"context"
	"fmt"
	"net"
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

// TestMeshSmoke is the third (and heaviest) Multipass integration test:
// it runs provisioning + security + mesh (mesh-gateway + mesh-join) on
// three bare Multipass VMs and asserts, over SSH, that all three nodes
// have joined a common headscale mesh and can reach each other over
// the tailnet.
//
// Phases explicitly NOT run: gateway, channels, node, agents. The
// gateway/node blocks in manifest-mesh.yml exist solely to let
// mesh.BuildMeshTargets figure out which machines participate in the
// mesh and which one hosts the control plane (install-headscale only
// runs on gateway.reference).
//
// Topology:
//
//	┌─────────────────┐       ┌─────────────────┐
//	│   node-host-1   │───────│  gateway-host   │── headscale :8080
//	│   tailscaled    │       │  headscale      │
//	└────────┬────────┘       └────────┬────────┘
//	         │                         │
//	         │     tailnet (100.64.0.0/10)
//	         │                         │
//	         │        ┌─────────────────┐
//	         └────────│   node-host-2   │
//	                  │   tailscaled    │
//	                  └─────────────────┘
//
// Why a single-stage apply now works:
//
//	The headscale control URL is a static string in the manifest —
//	`http://gateway-host.local:8080` — because every Multipass VM we
//	launch advertises its machine name over mDNS. That's not magic:
//	provisioning's cloud-init (see buildCloudInit in
//	internal/hosting/multipass/cloudinit.go) pins the VM's hostname +
//	FQDN to the manifest's machine name, installs avahi-daemon +
//	libnss-mdns, and pre-allows UDP 5353 in ufw so the later security
//	phase's `ufw enable` doesn't cut off multicast. Peer VMs on the
//	same Multipass bridge resolve `gateway-host.local` without us ever
//	needing to learn its LAN IP at test time — which in turn means the
//	whole "run provisioning, read the IP, patch the manifest, run
//	everything again" choreography this test used to do is gone.
//
// Runtime budget: provisioning ~60s (adds ~15s for avahi install over
// the other tests) + security ~35s + mesh ~60–90s for three VMs +
// tailscale discovery ~15s = ~4 minutes green-path. The 15-minute cap
// handles cold apt caches on fresh images.
func TestMeshSmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-mesh.yml")
	m.Prefix = "it-mesh-" + randSuffix(t)
	prefix := m.Prefix
	// See rewriteMeshHost — the fixture's control URL references
	// `gateway-host.local` but cloud-init pins the VM hostname to
	// `<prefix>-gateway-host`, so we realign them before plan build.
	rewriteMeshHost(m)

	// Fixture sanity — if the YAML drifts in a way that breaks the
	// topology this test depends on, fail loud at load rather than at
	// some confusing SSH error 90 seconds in.
	if len(m.Machines) != 3 {
		t.Fatalf("fixture sanity: expected 3 machines, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	if len(m.Nodes) != 2 {
		t.Fatalf("fixture sanity: expected 2 nodes, got %d", len(m.Nodes))
	}
	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeMultipass {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
		}
	}
	gw := &m.Gateways[0]
	if gw.Networking == nil || !strings.EqualFold(gw.Networking.Mode, "headscale") {
		t.Fatalf("fixture sanity: gateway %q networking.mode must be 'headscale'", gw.Name)
	}
	// The whole point of this fixture refresh is a static mDNS control
	// URL — if someone strips it back out assuming sslip works on a
	// LAN, we'd silently fall back to `curl icanhazip.com` on the
	// gateway VM and chase ghosts. Fail loud.
	if gw.Networking.PublicHostname == nil ||
		!strings.EqualFold(gw.Networking.PublicHostname.Strategy, "custom") ||
		!strings.Contains(gw.Networking.PublicHostname.Host, ".local") {
		t.Fatalf("fixture sanity: gateway %q must set public_hostname.strategy=custom with a .local host, got %+v",
			gw.Name, gw.Networking.PublicHostname)
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
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

	// =======================================================================
	// Single apply — provisioning + security + mesh in one pass. Works
	// because the control URL is static (mDNS) and doesn't need to be
	// patched after we learn the gateway's LAN IP.
	// =======================================================================

	plan, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:  m,
		Provider:  prov,
		SSHPubKey: pubKey,
		SSHDial:   dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	// mesh.AddPhase splits mesh into two sub-phases when any gateway is
	// headscale. Assert that split actually happened in the plan we built;
	// if someone regresses mesh.AddPhase into a single "mesh" phase, our
	// OnlyPhases filter below would silently drop the whole phase and the
	// test would pass-but-not-test-mesh.
	for _, want := range []string{"provisioning", "security", "mesh-gateway", "mesh-join"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + mesh on all 3 VMs")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		// Explicit allowlist: gateway/channels/node/agents aren't
		// wired up in this fixture and would fail if we let the full
		// plan rip. OnlyPhases is the seam the plan builder exposes
		// for partial runs.
		OnlyPhases: []string{"provisioning", "security", "mesh-gateway", "mesh-join"},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// =======================================================================
	// Assertions — the whole point of this test.
	// =======================================================================

	// Collect each VM's tailscale IP by SSH'ing in as the agent user and
	// running `tailscale ip -4`. The agent user exists and has passwordless
	// sudo because provisioning.EnsureAgentUser ran in stage 1; it's the
	// identity mesh itself dialed as, so we know it works.
	peers := make([]meshPeer, 0, len(m.Machines))
	for _, mc := range m.Machines {
		mt := findMachineTarget(t, plan, mc.Name)
		if mt.Instance == nil || strings.TrimSpace(mt.Instance.PublicIPv4) == "" {
			t.Fatalf("post-apply: machine %q has no Instance IP", mc.Name)
		}
		lan := strings.TrimSpace(mt.Instance.PublicIPv4)
		ts := readTailscaleIP(t, dial, lan, mc)
		if ts == "" {
			t.Fatalf("machine %q did not join the tailnet (tailscale ip -4 empty)", mc.Name)
		}
		if !isTailnetIP(ts) {
			t.Errorf("machine %q tailscale IP %q is not in 100.64.0.0/10", mc.Name, ts)
		}
		t.Logf("machine %q → lan=%s tailnet=%s", mc.Name, lan, ts)
		peers = append(peers, meshPeer{name: mc.Name, lanIP: lan, tsIP: ts, agentUser: mc.AgentUser})
	}

	// Uniqueness check: all three tailnet IPs must be distinct. Headscale
	// sequential allocation guarantees this; if we see duplicates, the
	// preauth key was reused in an unexpected way or headscale's db is
	// corrupted. Cheap sanity check.
	seen := make(map[string]string, len(peers))
	for _, p := range peers {
		if other, dup := seen[p.tsIP]; dup {
			t.Errorf("tailnet IP collision: %q and %q both have %s", other, p.name, p.tsIP)
		}
		seen[p.tsIP] = p.name
	}

	// The actual "does the mesh work?" assertion: every pair of peers
	// must be reachable via `tailscale ping`. We use tailscale ping (not
	// plain ICMP) because:
	//   1. It's the authoritative "can these two tailscaled processes
	//      see each other through the mesh?" check — exercises the
	//      same code path production traffic would.
	//   2. It doesn't depend on ufw allowing ICMP (security phase doesn't
	//      explicitly allow it, so plain ping might be blocked).
	//   3. Its output ("pong from <peer>") gives us a clean string to
	//      grep for.
	//
	// Retry: tailscaled takes a few seconds after `tailscale up` to learn
	// peer routes. 30s total retry budget with a 2s floor is plenty; in
	// practice peers are reachable within 5–10s of the last one joining.
	for _, src := range peers {
		for _, dst := range peers {
			if src.name == dst.name {
				continue
			}
			if err := tailscalePingRetry(t, dial, src, dst, 30*time.Second); err != nil {
				t.Errorf("mesh connectivity: %s → %s (%s): %v",
					src.name, dst.name, dst.tsIP, err)
			} else {
				t.Logf("mesh connectivity: %s → %s (%s) OK", src.name, dst.name, dst.tsIP)
			}
		}
	}

	// Teardown — same belt-and-suspenders pattern as the other two tests.
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
// Mesh-test-local SSH helpers.
//
// We dial as the *agent* user here (as opposed to the security test's root)
// because this test's whole story is "the agent user is real and functional
// after mesh installs tailscale under it" — switching to root would bypass
// exactly the identity we want to prove works.
// ---------------------------------------------------------------------------

// sshRunAsAgent is the mesh-test counterpart to the security test's
// sshRunAsRoot. Each assertion opens its own session (no pool, no
// multiplexing) so failures pin cleanly to one command and a 20s
// per-call timeout keeps a hung VM from stalling the whole test.
func sshRunAsAgent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	client, err := dial(ctx, host, 22, user)
	if err != nil {
		return "", fmt.Errorf("dial %s@%s: %w", user, host, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// readTailscaleIP returns the 100.64.x.y tailnet address assigned to
// mc, or "" if the VM hasn't joined yet. `tailscale ip -4` can print
// multiple lines when the node has several addresses; take the first
// non-empty line, same as install-tailscale's own parsing.
func readTailscaleIP(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) string {
	t.Helper()
	out, err := sshRunAsAgent(t, dial, host, mc, "tailscale ip -4 2>/dev/null || true")
	if err != nil {
		t.Logf("readTailscaleIP %s: %v", mc.Name, err)
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// isTailnetIP is a cheap sanity filter — headscale's default allocation
// prefix is 100.64.0.0/10. If the address we read back is outside that
// range, something is misconfigured (wrong prefix, or we parsed a
// peer's IP by mistake).
func isTailnetIP(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() == nil {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(addr)
}

// meshPeer groups the handful of facts we need about each VM after
// the mesh comes up: its Multipass-LAN address (for SSH), its tailnet
// address (for ping-target), its display name (for log clarity), and
// which agent user to dial as (driven by the manifest so a future
// change to the fixture — say, agent_user: claw — doesn't quietly
// strand this test on hardcoded "agent"). Declared at package scope
// because it's shared between assertion code and tailscalePingRetry.
type meshPeer struct {
	name      string
	lanIP     string
	tsIP      string
	agentUser string
}

// tailscalePingRetry runs `tailscale ping` from src → dst with retries.
// Tailscaled needs a few seconds after the last peer joins before every
// peer has learned every other peer's endpoints, so the first ping can
// fail even when the mesh is healthy. We retry at a fixed 2s cadence
// until budget runs out.
//
// The ping itself uses `--c 1 --timeout 5s` so a single attempt won't
// swallow the whole budget if a peer is genuinely unreachable.
func tailscalePingRetry(t *testing.T, dial provisioning.SSHDialFunc, src, dst meshPeer, budget time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastOut string
	var lastErr error
	mc := manifestdata.Machine{AgentUser: src.agentUser}
	cmd := fmt.Sprintf("tailscale ping --c 1 --timeout 5s %q", dst.tsIP)
	for time.Now().Before(deadline) {
		out, err := sshRunAsAgent(t, dial, src.lanIP, mc, cmd)
		// `tailscale ping` exits 0 and prints "pong from <host>" on
		// success; on failure it exits non-zero with "direct connection
		// not established" or similar. We check both exit status and
		// output content — belt-and-suspenders against tailscale CLI
		// behaviour changes.
		if err == nil && strings.Contains(out, "pong from") {
			return nil
		}
		lastOut = out
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("no pong from %s after %s (last err=%v, last output=%q)",
		dst.tsIP, budget, lastErr, lastOut)
}
