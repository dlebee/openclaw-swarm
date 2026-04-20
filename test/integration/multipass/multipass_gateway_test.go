//go:build integration_multipass

package multipass

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
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// TestGatewaySmoke is the fourth Multipass integration test: it runs
// provisioning + security + gateway on a single-machine
// manifest-gateway.yml fixture and asserts, over SSH, that the
// openclaw gateway daemon is actually running, listening, configured
// for loopback bind, and has a paired local CLI device.
//
// Phases explicitly NOT run: mesh, channels, node, agents. The
// fixture deliberately omits the `networking:` block on the gateway
// so BuildMeshTargets returns nil and the auto-added mesh phase
// becomes a noop configure-mesh step. The OnlyPhases filter excludes
// mesh anyway, but leaving it out of the fixture also means we don't
// need avahi/mDNS setup — the smallest possible gateway exercise.
//
// What this exercises that none of the previous Multipass tests do:
//
//   - common.InstallNodejsStep: NodeSource repo add + apt install of
//     nodejs 22.x. First test hitting the Node runtime at all.
//   - common.InstallOpenclawStep: `sudo npm install -g openclaw` —
//     pulls the real package from the public npm registry (~300MB
//     node_modules on disk after install).
//   - gateway.BootstrapGatewayStep: `openclaw onboard
//     --non-interactive --install-daemon`. Generates the gateway
//     auth token, writes ~/.openclaw/openclaw.json, installs a
//     systemd --user unit, `systemctl --user enable --now`. Critically,
//     this requires linger to be enabled for the agent user
//     (loginctl enable-linger agent, set up by
//     provisioning.EnsureAgentUser) — if linger is broken the user
//     unit won't persist across the SSH session and the health check
//     will fail.
//   - gateway.ConfigureGatewayStep: writes gateway.mode=local +
//     gateway.bind=loopback via `openclaw config set --batch-json`,
//     creates /var/tmp/openclaw-compile-cache, writes the systemd env
//     drop-in with NODE_COMPILE_CACHE + OPENCLAW_NO_RESPAWN, restarts
//     the unit and waits for :18789 again.
//   - gateway.PairGatewayDeviceStep: the "trigger a pending pairing
//     request via `openclaw nodes list`, then approve it via
//     `openclaw devices approve <id>`" dance. First test exercising
//     the openclaw WebSocket device API.
//
// Runtime budget: ~60s provisioning (includes the cloudinit avahi
// install from the mDNS change — the Gateway fixture still benefits
// from it for the multipass `.local` hostname, even without mesh) +
// ~90s security + ~4–6 min gateway (NodeSource repo add, apt install
// nodejs, npm install -g openclaw, onboard, restart, pair). 20min cap
// absorbs cold apt/npm caches on a fresh daily image.
func TestGatewaySmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-gateway.yml")
	m.Prefix = "it-gw-" + randSuffix(t)
	prefix := m.Prefix

	if len(m.Machines) != 1 {
		t.Fatalf("fixture sanity: expected 1 machine, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	if len(m.Nodes) != 0 {
		t.Fatalf("fixture sanity: expected 0 nodes, got %d", len(m.Nodes))
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
	gw := &m.Gateways[0]
	// The whole point of this minimal fixture is loopback bind — if
	// someone adds a networking block the gateway phase starts
	// requiring OPENCLAW_ALLOW_INSECURE_PRIVATE_WS and expecting a
	// mesh IP to exist, and the test stops being the smallest
	// gateway-only exercise. Fail loud instead.
	if gw.Networking != nil {
		t.Fatalf("fixture sanity: gateway %q must not have a networking block (loopback bind only), got %+v",
			gw.Name, gw.Networking)
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	// A regression that dropped install-nodejs or install-openclaw
	// from the gateway phase would make this test silently pass-
	// but-not-test-the-gateway; guard against that.
	for _, want := range []string{"provisioning", "security", "gateway"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + gateway on 1 VM")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		// Explicit allowlist: mesh phase is auto-added (as a noop)
		// but there's no reason to run it, and leaving it out keeps
		// the runtime envelope predictable.
		OnlyPhases: []string{"provisioning", "security", "gateway"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- assertions ---------------------------------------------------------

	mc := m.Machines[0]
	mt := findMachineTarget(t, plan, mc.Name)
	if mt.Instance == nil {
		t.Fatalf("machine %q has no Instance after apply", mc.Name)
	}
	inst := mt.Instance
	if inst.Status != "running" {
		t.Errorf("machine %q status = %q, want %q", mc.Name, inst.Status, "running")
	}
	if strings.TrimSpace(inst.PublicIPv4) == "" || net.ParseIP(inst.PublicIPv4) == nil {
		t.Fatalf("machine %q PublicIPv4 %q is not a valid IP", mc.Name, inst.PublicIPv4)
	}
	t.Logf("machine %q → label=%s ip=%s status=%s",
		mc.Name, inst.Label, inst.PublicIPv4, inst.Status)

	host := inst.PublicIPv4

	// ---- gateway-phase outside-in assertions -----------------------------------
	//
	// SSH as the agent user (the identity the gateway phase dialed
	// as). The user-mode systemd unit, config file, device pairing
	// state — all of it lives in /home/agent, not /root. Any
	// assertion that tries to SSH as root here would see an empty
	// ~/.openclaw and a dormant `systemctl --user`.

	// Node + openclaw CLIs: prerequisite for every other assertion
	// below, so check these first and bail hard if they're missing —
	// running subsequent asserts against a box with no openclaw
	// binary just prints confusing "command not found" errors.
	assertNodeInstalled(t, dial, host, mc)
	assertOpenclawInstalled(t, dial, host, mc)

	// Systemd --user + health port: the canonical "is the daemon
	// actually running?" check. systemctl is-active catches the
	// "unit enabled but crashing" case that plain port-listen
	// wouldn't; the port check catches the "unit active but not yet
	// bound" case in the opposite direction. Both must hold.
	assertGatewayUnitActive(t, dial, host, mc)
	assertGatewayPortListening(t, dial, host, mc)

	// Config file exists on disk (openclaw.json), has the gateway
	// bind set to loopback (matches the nil-networking fixture), and
	// gateway.mode=local (configure-gateway's canonical value). A
	// successful health check could hide a drift here — the daemon
	// would still listen — so we read the config explicitly.
	assertOpenclawConfigExists(t, dial, host, mc)
	assertOpenclawConfigValue(t, dial, host, mc, "gateway.mode", "local")
	assertOpenclawConfigValue(t, dial, host, mc, "gateway.bind", "loopback")

	// Startup-optim env drop-in. ConfigureGatewayStep's Verify
	// already checks this, but the whole point of the outside-in
	// assertions is to not trust the step. Read the drop-in
	// directly from /etc/systemd/user/<unit>.service.d/.
	assertGatewayEnvDropIn(t, dial, host, mc, "NODE_COMPILE_CACHE", "/var/tmp/openclaw-compile-cache")
	assertGatewayEnvDropIn(t, dial, host, mc, "OPENCLAW_NO_RESPAWN", "1")

	// Device pairing: one paired local device, zero pending.
	// `openclaw devices list --json` prints the canonical shape.
	assertGatewayDevicePaired(t, dial, host, mc)

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
// gateway-phase assertion helpers
//
// Dial-as-agent is the key difference from the security tier (which
// dials as root): openclaw's systemd --user unit and ~/.openclaw/
// live under whichever account ran the onboarding, and that's the
// agent user. Root wouldn't see the unit without a manual
// XDG_RUNTIME_DIR override, and we'd rather test the same path
// production operators would hit.
// ---------------------------------------------------------------------------

// sshRunAsGatewayAgent mirrors the mesh test's sshRunAsAgent but
// hardcoded to 45s because `openclaw devices list --json` can take
// ~10s on a cold node-compile-cache (first invocation pays the full
// TypeScript startup tax before the cache warms). 45s leaves plenty
// of headroom without letting a genuinely hung command stall the
// whole test run.
func sshRunAsGatewayAgent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

// assertNodeInstalled is the prerequisite check for everything else
// in the gateway phase. A missing `node` binary means install-nodejs
// silently no-op'd (its Check short-circuits when `node --version`
// succeeds, so a prior install that left a broken binary would fool
// it). We look for a major version ≥ v22 because the NodeSource
// script is pinned to setup_22.x; v20 here would signal a drift in
// install-nodejs.
func assertNodeInstalled(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, `node --version 2>&1`)
	if err != nil {
		t.Errorf("[%s] node --version: %v\n%s", mc.Name, err, out)
		return
	}
	if !strings.HasPrefix(out, "v") {
		t.Errorf("[%s] node --version = %q, want v<major>.<minor>.<patch>", mc.Name, out)
		return
	}
	// Soft check: NodeSource setup_22.x should land v22.x. A major-
	// version regression is a clear install-nodejs fault.
	if !strings.HasPrefix(out, "v22.") {
		t.Logf("[%s] node version %q (expected v22.x from NodeSource setup_22.x)", mc.Name, out)
	}
}

func assertOpenclawInstalled(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, `openclaw --version 2>&1`)
	if err != nil {
		t.Errorf("[%s] openclaw --version: %v\n%s", mc.Name, err, out)
		return
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("[%s] openclaw --version returned empty output", mc.Name)
	}
}

// assertGatewayUnitActive checks the user-mode systemd unit state.
// `systemctl --user is-active` only works reliably when
// XDG_RUNTIME_DIR is populated; linger (set by
// provisioning.EnsureAgentUser) keeps /run/user/<uid> alive across
// SSH sessions. If this fails with "Failed to connect to bus" the
// regression is almost certainly in linger / enable-linger.
func assertGatewayUnitActive(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	cmd := `export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user is-active openclaw-gateway 2>&1`
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, cmd)
	if out != "active" {
		t.Errorf("[%s] systemctl --user is-active openclaw-gateway = %q (err=%v), want \"active\"",
			mc.Name, out, err)
	}
}

// assertGatewayPortListening is the "daemon actually bound" cross-
// check. :18789 is the canonical gateway port — a systemd unit can
// report active and still be stuck on startup (TypeScript compile,
// config validation, etc.) without actually listening. ss -tln is
// preferred over netstat because it doesn't require extra packages
// on a fresh ubuntu24.04 image.
func assertGatewayPortListening(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	// Retry because the gateway unit was just restarted (by
	// configure-gateway). bootstrap → configure → restart leaves
	// a ~5s window where the daemon isn't yet listening even though
	// configure-gateway's RestartAndWait returned.
	deadline := time.Now().Add(30 * time.Second)
	var lastOut string
	for time.Now().Before(deadline) {
		out, _ := sshRunAsGatewayAgent(t, dial, host, mc,
			`ss -tln 2>/dev/null | awk '{print $4}' | grep -E ':18789$' || true`)
		if out != "" {
			t.Logf("[%s] :18789 listener: %s", mc.Name, out)
			// Loopback bind: the listener must be on 127.0.0.1,
			// not 0.0.0.0 — that's the whole point of omitting
			// the networking block.
			if !strings.HasPrefix(out, "127.0.0.1:") && !strings.HasPrefix(out, "[::1]:") {
				t.Errorf("[%s] :18789 is listening but not on loopback: %q", mc.Name, out)
			}
			return
		}
		lastOut = out
		time.Sleep(2 * time.Second)
	}
	t.Errorf("[%s] :18789 never started listening (last ss output: %q)", mc.Name, lastOut)
}

// assertOpenclawConfigExists confirms bootstrap-gateway actually
// wrote ~/.openclaw/openclaw.json. This is Check()'s signal to
// short-circuit on re-runs, so a missing file means a future
// re-apply would try to re-onboard from scratch.
func assertOpenclawConfigExists(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc,
		`test -f "$HOME/.openclaw/openclaw.json" && echo ok || echo missing`)
	if err != nil {
		t.Errorf("[%s] stat ~/.openclaw/openclaw.json: %v\n%s", mc.Name, err, out)
		return
	}
	if out != "ok" {
		t.Errorf("[%s] ~/.openclaw/openclaw.json missing", mc.Name)
	}
}

func assertOpenclawConfigValue(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, key, want string) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc,
		fmt.Sprintf(`openclaw config get %s 2>&1`, key))
	if err != nil {
		t.Errorf("[%s] openclaw config get %s: %v\n%s", mc.Name, key, err, out)
		return
	}
	got := strings.TrimSpace(out)
	if got != want {
		t.Errorf("[%s] openclaw config get %s = %q, want %q", mc.Name, key, got, want)
	}
}

// assertGatewayEnvDropIn reads the systemd env drop-in written by
// configure-gateway directly from disk. systemd user units store
// drop-ins under ~/.config/systemd/user/<unit>.service.d/ (not
// /etc/systemd/...) — that's a common source of confusion when
// debugging by hand. We grep instead of parsing because
// `systemctl --user show -p Environment` merges all env sources and
// wouldn't prove the drop-in file itself is correct.
func assertGatewayEnvDropIn(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, key, wantVal string) {
	t.Helper()
	// The drop-in path — ConfigureGatewayStep writes it via
	// systemd.WriteEnvDropIn with userMode=true, which puts it
	// under $XDG_CONFIG_HOME/systemd/user (= ~/.config/systemd/user
	// on default setups).
	cmd := `grep -H '^Environment=' "$HOME/.config/systemd/user/openclaw-gateway.service.d/"*.conf 2>/dev/null || ` +
		`grep -H '^Environment=' /etc/systemd/user/openclaw-gateway.service.d/*.conf 2>/dev/null || ` +
		`echo missing`
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, cmd)
	if err != nil {
		t.Errorf("[%s] read env drop-in: %v\n%s", mc.Name, err, out)
		return
	}
	want := fmt.Sprintf(`%s=%s`, key, wantVal)
	if !strings.Contains(out, want) {
		t.Errorf("[%s] env drop-in missing %q; got:\n%s", mc.Name, want, out)
	}
}

// assertGatewayDevicePaired confirms pair-gateway-device got its job
// done: at least one paired local device and zero pending. Zero
// pending matters because a leftover pending entry would mean
// ApproveDevice silently failed on a previous attempt; a subsequent
// `openclaw agents add` would then prompt for approval and block.
func assertGatewayDevicePaired(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc,
		`openclaw devices list --json 2>/dev/null`)
	if err != nil {
		t.Errorf("[%s] openclaw devices list --json: %v\n%s", mc.Name, err, out)
		return
	}
	var dl struct {
		Pending []struct {
			RequestID string `json:"requestId"`
		} `json:"pending"`
		Paired []struct {
			DisplayName string `json:"displayName"`
			ClientMode  string `json:"clientMode"`
		} `json:"paired"`
	}
	if err := json.Unmarshal([]byte(out), &dl); err != nil {
		t.Errorf("[%s] parse devices list: %v\nraw output:\n%s", mc.Name, err, out)
		return
	}
	if len(dl.Paired) < 1 {
		t.Errorf("[%s] expected >=1 paired device, got %d (raw: %s)", mc.Name, len(dl.Paired), out)
	}
	if len(dl.Pending) != 0 {
		t.Errorf("[%s] expected 0 pending devices, got %d (pair-gateway-device should have approved them all)",
			mc.Name, len(dl.Pending))
	}
	t.Logf("[%s] devices: paired=%d pending=%d", mc.Name, len(dl.Paired), len(dl.Pending))
}
