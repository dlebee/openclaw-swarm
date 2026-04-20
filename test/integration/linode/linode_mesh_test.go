//go:build integration_linode

package linode

import (
	"context"
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

// TestMeshSmoke is the heaviest (and most expensive) Linode
// integration test: provisioning + security + mesh-gateway +
// mesh-join on three real Linode instances, asserting over SSH that
// all three nodes have joined a common headscale mesh and can reach
// each other across the tailnet.
//
// Phases explicitly NOT run: gateway, channels, node, agents. The
// gateway + nodes blocks in manifest-mesh.yml exist solely so
// mesh.BuildMeshTargets can figure out which machines participate in
// the mesh and which one hosts the control plane (install-headscale
// only runs on gateway.reference).
//
// What this tier uniquely exercises (vs. TestMeshSmoke in the
// Multipass tier):
//
//   - sslip.io public_hostname strategy — the whole
//     resolveControlURL → gatewayPublicIP → sslip.io URL chain.
//     Multipass uses `custom` + mDNS, which never touches sslip.
//   - install-caddy + ACME (Let's Encrypt HTTP-01) cert issuance.
//     InstallCaddyStep.Applicable gates on the control URL scheme
//     being HTTPS, which it only is for sslip/custom-HTTPS — never
//     for the Multipass fixture.
//   - headscale-over-HTTPS end to end: tailscaled dials
//     https://mesh-gw.<ip>.sslip.io, Caddy terminates TLS with the
//     Let's Encrypt cert, Headscale speaks plain HTTP on :8080.
//
// Topology (identical to the Multipass tier):
//
//	┌─────────────────┐       ┌─────────────────┐
//	│   node-host-1   │───────│  gateway-host   │── headscale :8080 (via Caddy on :443)
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
// Runtime budget: provisioning ~6 min (3 instances in parallel) +
// security ~6 min (apt) + mesh ~8 min (install-caddy + ACME cert
// issuance + install-headscale + preauth-key + 3× install-tailscale).
// ACME can take 30–120s in the best case or block for minutes if
// Let's Encrypt's HTTP-01 challenger is slow to hit the gateway, so
// 30 min is the ceiling.
//
// Cost envelope (g6-standard-1 @ $0.015/hr × 3 × 0.5h) ≈ $0.025.
func TestMeshSmoke(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-mesh.yml")
	m.Prefix = "it-lin-mesh-" + randSuffix(t)
	prefix := m.Prefix

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
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
	}
	gw := &m.Gateways[0]
	if gw.Networking == nil || !strings.EqualFold(gw.Networking.Mode, "headscale") {
		t.Fatalf("fixture sanity: gateway %q networking.mode must be 'headscale'", gw.Name)
	}
	// The whole point of the Linode tier is to exercise the sslip
	// strategy end to end. If someone switches the fixture to
	// `custom` + a .local host they'd silently retarget mDNS (which
	// doesn't work on public Linode IPs) and chase cert errors for
	// days. Fail loud instead.
	if gw.Networking.PublicHostname != nil &&
		strings.TrimSpace(gw.Networking.PublicHostname.Strategy) != "" &&
		!strings.EqualFold(gw.Networking.PublicHostname.Strategy, "sslip") {
		t.Fatalf("fixture sanity: gateway %q must use public_hostname.strategy=sslip (or leave it unset), got %q",
			gw.Name, gw.Networking.PublicHostname.Strategy)
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

	// =======================================================================
	// Single apply — provisioning + security + mesh in one pass.
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

	// mesh.AddPhase splits mesh into two sub-phases when any gateway
	// is headscale. Assert that split actually happened in the plan
	// we built; a regression that collapsed mesh back to a single
	// "mesh" phase would make our OnlyPhases filter silently drop it.
	for _, want := range []string{"provisioning", "security", "mesh-gateway", "mesh-join"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + mesh on all 3 Linode instances")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		OnlyPhases: []string{
			"provisioning", "security",
			"mesh-gateway", "mesh-join",
		},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// =======================================================================
	// Assertions — the whole point of this test.
	// =======================================================================

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
		t.Logf("machine %q → public=%s tailnet=%s", mc.Name, lan, ts)
		peers = append(peers, meshPeer{name: mc.Name, publicIP: lan, tsIP: ts, agentUser: mc.AgentUser})
	}

	// Uniqueness check: all three tailnet IPs must be distinct.
	// Headscale's sequential allocation guarantees this; duplicates
	// mean the preauth key was reused in an unexpected way or the
	// headscale db is corrupted. Cheap sanity check.
	seen := make(map[string]string, len(peers))
	for _, p := range peers {
		if other, dup := seen[p.tsIP]; dup {
			t.Errorf("tailnet IP collision: %q and %q both have %s", other, p.name, p.tsIP)
		}
		seen[p.tsIP] = p.name
	}

	// The actual "does the mesh work?" assertion: every pair of peers
	// must be reachable via `tailscale ping`. See the Multipass tier's
	// TestMeshSmoke for the full rationale — tailscale ping over
	// plain ICMP, retry because tailscaled takes a few seconds after
	// the last peer joins before every peer has learned every other
	// peer's endpoints.
	for _, src := range peers {
		for _, dst := range peers {
			if src.name == dst.name {
				continue
			}
			if err := tailscalePingRetry(t, dial, src, dst, 60*time.Second); err != nil {
				t.Errorf("mesh connectivity: %s → %s (%s): %v",
					src.name, dst.name, dst.tsIP, err)
			} else {
				t.Logf("mesh connectivity: %s → %s (%s) OK", src.name, dst.name, dst.tsIP)
			}
		}
	}

	// Green-path teardown — belt-and-suspenders with t.Cleanup.
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
// Mesh-test-local SSH helpers — mirror the Multipass tier's set.
//
// Dial as the *agent* user (per the fixture) so this test confirms
// the end state the rest of claws expects: `tailscale` works under
// agent_user without a sudo prompt.
// ---------------------------------------------------------------------------

func sshRunAsAgent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
// non-empty line.
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

// isTailnetIP — headscale's default allocation prefix is
// 100.64.0.0/10. If the address we read back is outside that range,
// something is misconfigured (wrong prefix, or we parsed a peer's IP
// by mistake).
func isTailnetIP(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() == nil {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(addr)
}

// meshPeer groups the handful of facts we need about each instance
// after the mesh comes up: its public Linode address (for SSH), its
// tailnet address (for ping-target), its display name (for log
// clarity), and which agent user to dial as (driven by the manifest
// so a future change to the fixture doesn't silently strand this
// test on a hardcoded "agent").
type meshPeer struct {
	name      string
	publicIP  string
	tsIP      string
	agentUser string
}

// tailscalePingRetry runs `tailscale ping` from src → dst with
// retries. Over the public internet (no shared LAN), tailscaled's
// initial peer discovery + DERP probe + direct-connection attempts
// all play out across several seconds; the first ping can fail even
// when the mesh is healthy. 60s total retry budget (vs. the
// Multipass tier's 30s) accounts for DERP relay fallback latency on
// cross-region-but-same-account peers.
func tailscalePingRetry(t *testing.T, dial provisioning.SSHDialFunc, src, dst meshPeer, budget time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastOut string
	var lastErr error
	mc := manifestdata.Machine{AgentUser: src.agentUser}
	cmd := fmt.Sprintf("tailscale ping --c 1 --timeout 5s %q", dst.tsIP)
	for time.Now().Before(deadline) {
		out, err := sshRunAsAgent(t, dial, src.publicIP, mc, cmd)
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
