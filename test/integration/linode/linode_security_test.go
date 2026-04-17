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

// TestSecuritySmoke is the Linode counterpart to the Multipass tier's
// TestSecuritySmoke. It runs provisioning + security on the
// two-machine manifest-security.yml fixture and asserts, over SSH,
// that the hardening actually took effect on the live instances.
//
// What this exercises that TestProvisioningSmoke doesn't:
//
//   - ensure-agent-user's real useradd path (agent_user=agent distinct
//     from bootstrap_user=root).
//   - install-security-packages: apt install of ufw + fail2ban +
//     unattended-upgrades on a stock linode/ubuntu24.04 image.
//   - enable-ufw: UFW enabled with the SSH port allowed — critically,
//     this must NOT lock us out of the Linode (UFW on a public-IP box
//     with the SSH rule missing would be a catastrophic regression).
//   - enable-fail2ban: jail.local written, fail2ban service active.
//   - enable-unattended-upgrades: systemd unit enabled.
//
// Each step's own Verify method gets hit by the plan execution itself;
// the assertions below are the outside-in cross-check. SSH as root
// (the bootstrap user, which just ran the apt/ufw/systemctl work) and
// ask the live box directly.
//
// Runtime budget: ~5 min provisioning + ~5 min security (apt install
// + service starts on two parallel standard-1 instances). The
// 15-minute cap absorbs cold apt mirrors and Linode region slowness.
func TestSecuritySmoke(t *testing.T) {
	tok := loadLinodeToken(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-security.yml")
	m.Prefix = "it-lin-sec-" + randSuffix(t)
	prefix := m.Prefix
	if len(m.Machines) < 1 {
		t.Fatalf("fixture sanity: expected at least one machine, got 0")
	}
	for i, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %d (%q) type = %q, want %q",
				i, mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
		}
		// Linode's root is the bootstrap user, not the cloud-image
		// default. If someone edits agent_user to "root" the
		// ensure-agent-user step becomes a no-op and this test stops
		// proving anything about the useradd path — fail loudly.
		if mc.AgentUser == "root" {
			t.Fatalf("fixture sanity: machine %q agent_user is 'root' — "+
				"this test needs a dedicated agent account to exercise "+
				"ensure-agent-user's useradd path", mc.Name)
		}
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov := linode.NewProvider(tok)
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
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

	for _, want := range []string{"provisioning", "security"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning", "security"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- assertions ---------------------------------------------------------

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

		// Outside-in security assertions — SSH as the bootstrap user
		// (root, per the fixture) because it's the identity that just
		// ran apt/ufw/systemctl. Asking the VM directly is the whole
		// point of an integration test.
		assertAgentUserExists(t, dial, inst.PublicIPv4, mc)
		assertPackageInstalled(t, dial, inst.PublicIPv4, mc, "ufw")
		assertPackageInstalled(t, dial, inst.PublicIPv4, mc, "fail2ban")
		assertPackageInstalled(t, dial, inst.PublicIPv4, mc, "unattended-upgrades")
		assertUFWActive(t, dial, inst.PublicIPv4, mc)
		assertSystemdActive(t, dial, inst.PublicIPv4, mc, "fail2ban")
		assertSystemdEnabled(t, dial, inst.PublicIPv4, mc, "unattended-upgrades")
	}

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

	for _, inst := range insts {
		if err := prov.DeleteInstance(ctx, inst.ResourceID); err != nil {
			t.Fatalf("DeleteInstance %s: %v", inst.Label, err)
		}
	}
	if after := waitForEmptyListByTag(ctx, t, prov, "claws/"+prefix, 30*time.Second); len(after) != 0 {
		t.Errorf("ListByTag after destroy still returned %d instances: %+v", len(after), after)
	}
}

// ---------------------------------------------------------------------------
// assertion helpers — SSH-over-root to inspect the live instance
// ---------------------------------------------------------------------------

// sshRunAsRoot dials the instance as root (the bootstrap user in this
// test's fixture), runs a single command, and returns combined
// stdout+stderr plus the underlying error. Same pattern as the
// Multipass tier's sshRunAsRoot; the production SSH pool is not used
// here because these assertions run after plan execution has fully
// completed and returned its pooled clients.
//
// 30s per-assert timeout is generous: Linode network latency plus a
// freshly-configured UFW ruleset occasionally means the first syn
// sees a couple of retransmits before sshd's allow rule pins in.
func sshRunAsRoot(t *testing.T, dial provisioning.SSHDialFunc, host, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

// assertPackageInstalled checks dpkg's database directly.
// `apt install` is idempotent but can silently succeed without
// actually installing (e.g. unresolvable dependencies getting skipped
// with a warning), so the authoritative signal is dpkg's status
// field. Anything other than "install ok installed" means the
// package is not usable.
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
// a failure. We explicitly don't grep for a specific port rule — the
// SSH allow port depends on Machine.SSHPort and enable-ufw's own
// Verify already covers that.
func assertUFWActive(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsRoot(t, dial, host, "ufw status | head -n1")
	if err != nil {
		t.Errorf("[%s] ufw status: %v\n%s", mc.Name, err, out)
		return
	}
	if !strings.Contains(out, "Status: active") {
		t.Errorf("[%s] ufw status line = %q, want to contain 'Status: active'", mc.Name, out)
	}
}

func assertSystemdActive(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, unit string) {
	t.Helper()
	out, err := sshRunAsRoot(t, dial, host, fmt.Sprintf("systemctl is-active %q", unit))
	if out != "active" {
		t.Errorf("[%s] systemctl is-active %s = %q (err=%v), want \"active\"",
			mc.Name, unit, out, err)
	}
}

// assertSystemdEnabled is the right check for unattended-upgrades —
// it's a timer-driven unit that doesn't necessarily report "active"
// on a freshly-booted instance (it's waiting for the next scheduled
// run), but must be enabled so systemd fires it later.
func assertSystemdEnabled(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, unit string) {
	t.Helper()
	out, err := sshRunAsRoot(t, dial, host, fmt.Sprintf("systemctl is-enabled %q", unit))
	switch out {
	case "enabled", "enabled-runtime", "alias", "static":
	default:
		t.Errorf("[%s] systemctl is-enabled %s = %q (err=%v), want one of: enabled, enabled-runtime, alias, static",
			mc.Name, unit, out, err)
	}
}
