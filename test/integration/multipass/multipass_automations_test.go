//go:build integration_multipass

package multipass

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

const automationSmokeManifest = "manifest-automations-smoke.yml"

// TestProvisioningSecurityAutomationSmoke runs provisioning + security +
// one non-manual automation on two Multipass VMs. It asserts the default
// apply plan includes the automation phase (named after the automation) and
// does not register manual-only automations. After apply, it SSHs as the
// agent user and verifies the automation dropped its marker file; the manual
// automation's marker must be absent.
func TestProvisioningSecurityAutomationSmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	m := loadTestManifest(t, automationSmokeManifest)
	m.Prefix = "it-auto-" + randSuffix(t)
	prefix := m.Prefix

	manifestPath, err := filepath.Abs(filepath.Join("testdata", automationSmokeManifest))
	if err != nil {
		t.Fatalf("abs manifest path: %v", err)
	}

	if len(m.Machines) < 1 {
		t.Fatalf("fixture sanity: expected at least one machine, got 0")
	}
	for i, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeMultipass {
			t.Fatalf("fixture sanity: machine %d (%q) type = %q, want %q",
				i, mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
		}
	}

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}

	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
			t.Logf("CLAWS_IT_KEEP_VMS set → leaving VMs up for debug (prefix=%s)", prefix)
			return
		}
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

	plan, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:     m,
		ManifestPath: manifestPath,
		Provider:     prov,
		SSHPubKey:    pubKey,
		SSHDial:      dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	phases := plan.PhaseNames()
	for _, want := range []string{"provisioning", "security", "it-nonmanual-auto"} {
		if !containsStr(phases, want) {
			t.Fatalf("plan is missing %q phase; got %v", want, phases)
		}
	}
	if containsStr(phases, "it-manual-never") {
		t.Fatalf("plan incorrectly includes manual automation phase %q; got %v",
			"it-manual-never", phases)
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		OnlyPhases: []string{
			"provisioning",
			"security",
			"it-nonmanual-auto",
		},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

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
			t.Errorf("machine %q PublicIPv4 is empty", mc.Name)
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

		assertAutomationMarkerOK(t, dial, inst.PublicIPv4, mc)
		assertManualAutomationMarkerAbsent(t, dial, inst.PublicIPv4, mc)
	}

	insts, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after apply: %v", err)
	}
	if len(insts) != len(wantLabels) {
		t.Fatalf("ListByTag returned %d instances, want %d: %+v",
			len(insts), len(wantLabels), insts)
	}
}

// TestProvisioningSecurityAutomationIncludeManual mirrors `claws apply
// --include-manual-automations`: the first apply run uses the default plan
// (manual automations omitted); a second plan build with
// BuildOptions.IncludeManualAutomations=true adds phase "it-manual-never",
// which we execute alone to prove the flag wires through and the manual
// step runs on the already-provisioned VMs.
func TestProvisioningSecurityAutomationIncludeManual(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	m := loadTestManifest(t, automationSmokeManifest)
	m.Prefix = "it-automan-" + randSuffix(t)
	prefix := m.Prefix

	manifestPath, err := filepath.Abs(filepath.Join("testdata", automationSmokeManifest))
	if err != nil {
		t.Fatalf("abs manifest path: %v", err)
	}

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}

	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
			t.Logf("CLAWS_IT_KEEP_VMS set → leaving VMs up for debug (prefix=%s)", prefix)
			return
		}
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

	buildOpts := func(includeManual bool) planapply.BuildOptions {
		return planapply.BuildOptions{
			Manifest:                 m,
			ManifestPath:             manifestPath,
			Provider:                 prov,
			SSHPubKey:                pubKey,
			SSHDial:                  dial,
			IncludeManualAutomations: includeManual,
		}
	}

	// --- pass 1: default apply (same as TestProvisioningSecurityAutomationSmoke) ---

	plan1, err := planapply.BuildPlan(buildOpts(false))
	if err != nil {
		t.Fatalf("build plan (includeManual=false): %v", err)
	}
	if containsStr(plan1.PhaseNames(), "it-manual-never") {
		t.Fatalf("plan should omit manual automation without IncludeManualAutomations; got %v",
			plan1.PhaseNames())
	}

	ex1, err := plan1.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}
	if err := ex1.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		OnlyPhases: []string{
			"provisioning",
			"security",
			"it-nonmanual-auto",
		},
	}); err != nil {
		t.Fatalf("execute pass 1: %v", err)
	}

	hostByName := make(map[string]manifestdata.Machine, len(m.Machines))
	for _, mc := range m.Machines {
		mt := findMachineTarget(t, plan1, mc.Name)
		if mt.Instance == nil {
			t.Fatalf("machine %q has no Instance after pass 1", mc.Name)
		}
		inst := mt.Instance
		if strings.TrimSpace(inst.PublicIPv4) == "" {
			t.Fatalf("machine %q has empty PublicIPv4", mc.Name)
		}
		hostByName[mc.Name] = mc
		t.Logf("machine %q → ip=%s", mc.Name, inst.PublicIPv4)

		assertAutomationMarkerOK(t, dial, inst.PublicIPv4, mc)
		assertManualAutomationMarkerAbsent(t, dial, inst.PublicIPv4, mc)
	}

	// --- pass 2: include manual (equivalent to --include-manual-automations) ---

	plan2, err := planapply.BuildPlan(buildOpts(true))
	if err != nil {
		t.Fatalf("build plan (includeManual=true): %v", err)
	}
	ph2 := plan2.PhaseNames()
	if !containsStr(ph2, "it-manual-never") {
		t.Fatalf("plan with IncludeManualAutomations should include phase %q; got %v",
			"it-manual-never", ph2)
	}

	ex2, err := plan2.Build()
	if err != nil {
		t.Fatalf("plan2.Build: %v", err)
	}
	if err := ex2.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"it-manual-never"},
	}); err != nil {
		t.Fatalf("execute manual automation phase: %v", err)
	}

	gw, ok := hostByName["gateway-host"]
	if !ok {
		t.Fatalf("fixture missing gateway-host")
	}
	node, ok := hostByName["node-host"]
	if !ok {
		t.Fatalf("fixture missing node-host")
	}
	gwIP := findMachineTarget(t, plan1, "gateway-host").Instance.PublicIPv4
	nodeIP := findMachineTarget(t, plan1, "node-host").Instance.PublicIPv4

	assertManualAutomationMarkerPresent(t, dial, gwIP, gw)
	assertManualAutomationMarkerAbsent(t, dial, nodeIP, node)
}

func assertAutomationMarkerOK(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsUser(t, dial, host, mc.AgentUser,
		`test -f /tmp/claws-it-nonmanual-marker && grep -q nonmanual-ok /tmp/claws-it-nonmanual-marker`)
	if err != nil {
		t.Errorf("[%s] non-manual automation marker: %v\n%s", mc.Name, err, out)
	}
}

func assertManualAutomationMarkerAbsent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	// Manual automation only targeted gateway-host; node-host should never see the file.
	out, err := sshRunAsUser(t, dial, host, mc.AgentUser, `test ! -f /tmp/claws-it-manual-marker`)
	if err != nil {
		t.Errorf("[%s] manual automation marker must not exist: %v\n%s", mc.Name, err, out)
	}
}

func assertManualAutomationMarkerPresent(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	out, err := sshRunAsUser(t, dial, host, mc.AgentUser,
		`test -f /tmp/claws-it-manual-marker && grep -q bad /tmp/claws-it-manual-marker`)
	if err != nil {
		t.Errorf("[%s] manual automation marker after include-manual pass: %v\n%s", mc.Name, err, out)
	}
}

func sshRunAsUser(t *testing.T, dial provisioning.SSHDialFunc, host, user, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
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
