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

// TestSecuritySmoke is the second Multipass integration test: it runs
// provisioning + security on the two-machine manifest-security.yml
// fixture and asserts, over SSH, that the hardening actually took
// effect on the live VM.
//
// What this exercises that TestProvisioningSmoke doesn't:
//
//   - ensure-agent-user's real useradd path. manifest-security.yml sets
//     agent_user: agent (distinct from bootstrap_user: root), so the
//     step can't short-circuit on "user already exists" like it does
//     when agent_user == ubuntu.
//   - install-security-packages: apt install of ufw + fail2ban +
//     unattended-upgrades.
//   - enable-ufw: UFW enabled with the SSH port allowed.
//   - enable-fail2ban: jail.local written, fail2ban service active.
//   - enable-unattended-upgrades: systemd unit enabled.
//
// Each step's own Verify method gets hit by the plan execution itself;
// the assertions below are the outside-in cross-check: SSH in as root,
// ask the VM directly, confirm the world looks the way the security
// phase claimed.
//
// Runtime budget: ~45s provisioning (same as TestProvisioningSmoke) +
// ~60–120s security on parallel VMs (apt install + service starts).
// Cold apt cache on fresh images is the dominant variable; 12 minutes
// is comfortably above the worst case we've observed.
func TestSecuritySmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-security.yml")
	// Same prefix-randomization trick as the provisioning test: keeps
	// parallel runs from colliding on VM labels even though the fixture
	// on disk ships with a fixed `claws-it-multipass-sec` prefix for
	// humans to read.
	m.Prefix = "it-sec-" + randSuffix(t)
	prefix := m.Prefix
	if len(m.Machines) < 1 {
		t.Fatalf("fixture sanity: expected at least one machine, got 0")
	}
	for i, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeMultipass {
			t.Fatalf("fixture sanity: machine %d (%q) type = %q, want %q",
				i, mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
		}
		// The whole point of this fixture is that agent_user != the
		// cloud-image default user — if someone edits the YAML and
		// accidentally aligns them, ensure-agent-user stops exercising
		// useradd and this test degenerates into TestProvisioningSmoke
		// + a handful of apt asserts. Fail loudly instead.
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
		}
		if mc.AgentUser == "ubuntu" {
			t.Fatalf("fixture sanity: machine %q agent_user is 'ubuntu' — "+
				"this test needs a dedicated agent account to exercise "+
				"ensure-agent-user's useradd path", mc.Name)
		}
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}

	dial := sshDialFunc(signer)

	// Register cleanup BEFORE apply so a crash mid-execution still nukes
	// the partially-provisioned VMs. See TestProvisioningSmoke for the
	// same pattern — it's the only reliable way to avoid leaking VMs
	// when the test hits a hard fail.
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

	for _, want := range []string{"provisioning", "security"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	// Provisioning + security only. Mesh/gateway/node/agents/channels
	// would need a headscale server, node bindings, etc. — out of scope
	// for this test, and the whole point of OnlyPhases is that we can
	// slice the plan to exactly the surface we care about.
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning", "security"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- assertions ---------------------------------------------------------

	// Every machine must have an Instance with state=running and an IPv4,
	// same baseline as TestProvisioningSmoke.
	wantLabels := make(map[string]struct{}, len(m.Machines))
	for _, mc := range m.Machines {
		mt := findMachineTarget(t, plan, mc.Name)
		if mt.Instance == nil {
			t.Fatalf("machine %q has no Instance after apply", mc.Name)
		}
		inst := mt.Instance
		if inst.Status != "running" {
			t.Errorf("machine %q status = %q, want %q", mc.Name, inst.Status, "running")
		}
		if strings.TrimSpace(inst.PublicIPv4) == "" {
			t.Errorf("machine %q PublicIPv4 is empty; Instance: %+v", mc.Name, inst)
			continue
		}
		if net.ParseIP(inst.PublicIPv4) == nil {
			t.Errorf("machine %q PublicIPv4 %q does not parse as an IP",
				mc.Name, inst.PublicIPv4)
			continue
		}
		wantLabels[inst.Label] = struct{}{}
		t.Logf("machine %q → label=%s ip=%s status=%s",
			mc.Name, inst.Label, inst.PublicIPv4, inst.Status)

		// ---- security-phase outside-in assertions --------------------------
		//
		// We SSH in as the bootstrap user (root, per the fixture) because
		// it's the identity that just ran apt/ufw/systemctl — we know it
		// works and has the highest privilege. Asking the VM directly is
		// the whole point of an integration test: if sshd says fail2ban
		// is active, we trust that over the step's own Verify method.

		assertAgentUserExists(t, dial, inst.PublicIPv4, mc)
		assertPackageInstalled(t, dial, inst.PublicIPv4, mc, "ufw")
		assertPackageInstalled(t, dial, inst.PublicIPv4, mc, "fail2ban")
		assertPackageInstalled(t, dial, inst.PublicIPv4, mc, "unattended-upgrades")
		assertUFWActive(t, dial, inst.PublicIPv4, mc)
		assertSystemdActive(t, dial, inst.PublicIPv4, mc, "fail2ban")
		assertSystemdEnabled(t, dial, inst.PublicIPv4, mc, "unattended-upgrades")
	}

	// Tag round-trip assertion — same as TestProvisioningSmoke — to
	// confirm the sidecar store saw every VM. Cheap, catches a subtle
	// class of regressions where a post-provisioning step damages the
	// tags.
	insts, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after apply: %v", err)
	}
	if len(insts) != len(wantLabels) {
		t.Fatalf("ListByTag returned %d instances, want %d: %+v",
			len(insts), len(wantLabels), insts)
	}
	for _, inst := range insts {
		if _, ok := wantLabels[inst.Label]; !ok {
			t.Errorf("ListByTag returned unexpected label %q", inst.Label)
		}
	}

	// Active teardown in the green path so DeleteInstance gets exercised
	// here too. If this trips up, the t.Cleanup registered earlier is
	// still a backstop.
	for _, inst := range insts {
		if err := prov.DeleteInstance(ctx, inst.ResourceID); err != nil {
			t.Fatalf("DeleteInstance %s: %v", inst.Label, err)
		}
	}
	after, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after destroy: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("ListByTag after destroy returned %d instances, want 0: %+v", len(after), after)
	}
}

// ---------------------------------------------------------------------------
// assertion helpers — SSH-over-root to inspect the live VM
// ---------------------------------------------------------------------------

// sshRunAsRoot dials the VM as root (the bootstrap user in this test's
// fixture), runs a single command, and returns combined stdout+stderr
// plus the underlying error. We don't use the production SSH pool here
// because these assertions run after plan execution has fully completed
// and returned its pooled clients — a fresh dial per assert is simplest
// and keeps the test's error messages pinned to exactly one command.
//
// 20s per-assert timeout is deliberately generous: freshly-configured
// UFW rules occasionally force a handful of retries at the TCP layer
// before the sshd allow pins in cleanly.
func sshRunAsRoot(t *testing.T, dial provisioning.SSHDialFunc, host, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := dial(ctx, host, 22, "root")
	if err != nil {
		return "", fmt.Errorf("dial root@%s: %w", host, err)
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

// assertAgentUserExists confirms that provisioning.EnsureAgentUser did
// its job — `id <agent_user>` on the VM must return an exit-0 uid line.
// This is the real useradd cross-check: manifest-security.yml uses a
// dedicated `agent` account so the step can't no-op on "user already
// exists". If this fails, ensure-agent-user's Execute silently did
// nothing and the whole security story downstream is compromised.
func assertAgentUserExists(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsRoot(t, dial, host, fmt.Sprintf("id -u %q", mc.AgentUser))
	if err != nil {
		t.Errorf("[%s] id %s: %v\n%s", mc.Name, mc.AgentUser, err, out)
		return
	}
	if out == "" {
		t.Errorf("[%s] id %s returned empty output", mc.Name, mc.AgentUser)
	}
}

// assertPackageInstalled checks dpkg's database directly. `apt install`
// on Debian/Ubuntu is idempotent but can silently succeed without
// actually installing (e.g. unresolvable dependencies getting skipped
// with a warning), so the authoritative signal is dpkg's status field.
// Anything other than "install ok installed" means the package is not
// usable.
func assertPackageInstalled(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, pkg string) {
	t.Helper()
	cmd := fmt.Sprintf("dpkg-query -W -f='${Status}' %q 2>/dev/null || true", pkg)
	out, err := sshRunAsRoot(t, dial, host, cmd)
	if err != nil {
		t.Errorf("[%s] dpkg-query %s: %v", mc.Name, pkg, err)
		return
	}
	if out != "install ok installed" {
		t.Errorf("[%s] package %s status = %q, want %q",
			mc.Name, pkg, out, "install ok installed")
	}
}

// assertUFWActive confirms UFW is enforcing rules. `ufw status` prints
// "Status: active" when enabled; anything else (inactive, missing) is
// a failure. We explicitly don't grep for a specific port rule here —
// the SSH allow port depends on Machine.SSHPort (default 22) and
// enable-ufw's own Verify already covers that.
func assertUFWActive(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	// `ufw status` on inactive systems prints "Status: inactive", so
	// grep -c is a clean signal without false positives.
	out, err := sshRunAsRoot(t, dial, host, "ufw status | head -n1")
	if err != nil {
		t.Errorf("[%s] ufw status: %v\n%s", mc.Name, err, out)
		return
	}
	if !strings.Contains(out, "Status: active") {
		t.Errorf("[%s] ufw status line = %q, want to contain 'Status: active'", mc.Name, out)
	}
}

// assertSystemdActive is the "is the service running right now" check.
// Used for fail2ban; a plain "is-enabled" isn't enough because systemd
// happily enables a unit that will crash at start time.
func assertSystemdActive(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, unit string) {
	t.Helper()
	out, err := sshRunAsRoot(t, dial, host, fmt.Sprintf("systemctl is-active %q", unit))
	// systemctl exits non-zero for non-active states; treat the output as
	// authoritative. We only log the err for extra context on failure.
	if out != "active" {
		t.Errorf("[%s] systemctl is-active %s = %q (err=%v), want \"active\"",
			mc.Name, unit, out, err)
	}
}

// assertSystemdEnabled is the right check for unattended-upgrades —
// it's a timer-driven unit that doesn't necessarily report "active" on
// a freshly-booted VM (it's waiting for the next scheduled run), but
// must be enabled so systemd fires it later.
func assertSystemdEnabled(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, unit string) {
	t.Helper()
	out, err := sshRunAsRoot(t, dial, host, fmt.Sprintf("systemctl is-enabled %q", unit))
	switch out {
	case "enabled", "enabled-runtime", "alias", "static":
		// All acceptable: "enabled" is the normal case,
		// "enabled-runtime" for runtime-only enables,
		// "alias"/"static" happen when the unit ships enabled by the
		// packager (common for unattended-upgrades.service on newer
		// Ubuntu images where the unit is actually a timer trigger).
	default:
		t.Errorf("[%s] systemctl is-enabled %s = %q (err=%v), want one of: enabled, enabled-runtime, alias, static",
			mc.Name, unit, out, err)
	}
}
