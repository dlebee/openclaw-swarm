//go:build integration_multipass

package multipass

import (
	"context"
	"os"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// Per-phase timeout: generous enough that even a slow apt/npm cold
// cache on a fresh VM won't false-positive as a phase hang. The whole
// test is capped at 30 min by the outer context so a genuinely stuck
// phase still terminates within the CI envelope rather than blocking
// the runner indefinitely.
const multipassPhasesPerPhaseTimeout = 10 * time.Minute

// TestPhasesIsolatedNoCache applies each `claws apply` phase as a
// separate cold-start invocation: fresh BuildPlan, fresh Executable,
// fresh context.Context with only an empty EnsurePlanCache, no
// pre-seeded SSH pool (no provisioning.SetSSHPool call), no cross-
// phase state re-hydration (no provisioning.ResolveHostedInstances
// call). This simulates an operator running
//
//	claws apply --only <phase>
//
// as a series of separate cold CLI invocations against the same
// prefix. The production CLI does pre-seed an SSH pool (see
// internal/cli/commands/apply/apply.go — SetSSHPool immediately
// after EnsurePlanCache), so this test is strictly COLDER than a
// realistic `apply --only` run; a phase that fails here because of
// an nil-pool dereference is a test bug, not a production regression,
// and the phase code is expected to fall back to direct dial
// (common.BorrowSSH: "if pool := sshPool(ctx); pool != nil { ... }
// c, err := dial(...); return c, key, err").
//
// What this test surfaces that TestCronAgentWithNodeExec does NOT:
// cross-phase plan-cache coupling. The production apply pipeline
// runs all phases in one process with a single shared plan cache,
// so a step in phase B that reads a key set by phase A works even
// though the step has no infrastructure-side check for the value.
// If an operator later runs `apply --only B` as a separate cold
// process (brand-new plan cache, no phase A run in this invocation),
// that step fails. The intent of this test is to expose exactly
// those couplings before a production operator hits them.
//
// Phases under test are enumerated at runtime via plan.PhaseNames()
// rather than hard-coded, so adding or renaming a phase upstream
// doesn't silently skip it. The fixture intentionally carries
// machines + gateway + channels + nodes + agents so every
// conditional AddPhase call in apply.BuildPlan fires:
//
//   - provisioning  (always)
//   - security      (always)
//   - mesh-gateway  (headscale + sslip/custom public_hostname)
//   - mesh-join
//   - gateway       (manifest.Gateways non-empty)
//   - channels      (gateway.channels non-empty — telegram-main)
//   - node          (manifest.Nodes non-empty)
//   - agents        (manifest.Agents non-empty)
//
// Abort-on-first-failure: once a phase fails, later phases can't
// meaningfully run (VMs not provisioned, mesh not up, gateway not
// reachable, etc.), so t.FailNow halts the enclosing test. The
// first-failing phase name is the signal — it points at the
// specific cross-phase coupling (or the specific phase's cold-
// start readiness) that needs fixing.
//
// Cleanup: a single t.Cleanup registered BEFORE any apply scans
// the prefix tag with provider.ListByTag and deletes every
// matching instance, regardless of which phase aborted. CLAWS_IT_
// KEEP_VMS=1 bypasses cleanup for post-mortem debugging (matches
// the other tests in this package).
//
// Runtime budget: provisioning dominates (~2-3 min for 2 VMs).
// Later phases are either fast (security ~30s, mesh-join ~30s,
// agents ~20s) or fail-fast (a phase without hydrated state tends
// to blow up within the connect timeout rather than linger).
// 30 min overall cap absorbs apt/npm cold-cache worst case.
func TestPhasesIsolatedNoCache(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer outerCancel()

	// Channel token — consumed by the `channels` phase via the
	// telegram plugin's LookupEnvFromManifest → os.Getenv path
	// (internal/manifests/service/envlookup.go:23 checks os.Getenv
	// before consulting the manifest's env_file, so no env_file
	// entry is needed on the fixture). Fake value is fine: the
	// `openclaw channels add` step is fully offline (the telegram
	// plugin's onAccountConfigChanged only clears local polling
	// offsets). The daemon's first getUpdates poll against
	// api.telegram.org would 401, but this test never waits for a
	// poll — it only cares that the apply phase converges.
	t.Setenv("TELEGRAM_MAIN_BOT_TOKEN", "fake-phases-token")

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-phases.yml")
	m.Prefix = "it-phases-" + randSuffix(t)
	prefix := m.Prefix
	// See rewriteMeshHost — realign the fixture's custom control URL
	// with the `<prefix>-gateway-host` hostname cloud-init will pin.
	rewriteMeshHost(m)

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}
	dial := sshDialFunc(signer)

	// Cleanup runs once at the end regardless of which phase aborted.
	// Uses prov.ListByTag + DeleteInstance so a mid-phase failure
	// still tears everything down; no per-phase cleanup needed.
	t.Cleanup(func() {
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

	// --- discover phase list from a probe build -----------------------------
	//
	// Build the plan once up front just to enumerate PhaseNames().
	// We deliberately do NOT reuse this plan for execution: every
	// sub-test below rebuilds its own plan so each phase sees the
	// same "fresh BuildPlan + fresh Executable" contract. The probe
	// build is cheap (no SSH, no apply — just target + step wiring
	// in memory) so rebuilding per sub-test is not a runtime
	// concern.
	probePlan, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:  m,
		Provider:  prov,
		SSHPubKey: pubKey,
		SSHDial:   dial,
	})
	if err != nil {
		t.Fatalf("probe BuildPlan: %v", err)
	}
	phases := probePlan.PhaseNames()
	if len(phases) == 0 {
		t.Fatalf("probe plan produced zero phases (manifest=%q)", "manifest-phases.yml")
	}
	t.Logf("phases discovered: %v", phases)

	// Sanity: the fixture is supposed to produce every conditional
	// phase. If it doesn't we silently lose coverage, so fail loudly
	// and point at the missing one(s).
	for _, want := range []string{
		"provisioning", "security",
		"mesh-gateway", "mesh-join",
		"gateway", "channels", "node", "agents",
	} {
		if !containsStr(phases, want) {
			t.Fatalf("manifest-phases.yml fixture is missing %q phase; got %v", want, phases)
		}
	}

	// --- apply each phase in isolation -------------------------------------

	for i, phase := range phases {
		phaseIdx := i + 1
		ok := t.Run(phase, func(t *testing.T) {
			if err := outerCtx.Err(); err != nil {
				t.Fatalf("outer context already done: %v", err)
			}

			phaseCtx, cancel := context.WithTimeout(outerCtx, multipassPhasesPerPhaseTimeout)
			defer cancel()

			// Fresh BuildPlan per sub-test. This re-runs
			// BuildMachineTargets, producing fresh MachineTarget
			// pointers with Instance == nil. The only cross-phase
			// state that survives is what's on the actual VMs
			// (discoverable via provider.ListByTag, files and
			// systemd units on disk). Everything the production
			// run kept in-memory on the plan cache —
			// mesh.controlURL, mesh.preauthKey, SSH_POOL,
			// planCacheMachineMeshIP:<name>,
			// planCacheMachineHost:<name> — is unavailable.
			plan, err := planapply.BuildPlan(planapply.BuildOptions{
				Manifest:  m,
				Provider:  prov,
				SSHPubKey: pubKey,
				SSHDial:   dial,
			})
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			ex, err := plan.Build()
			if err != nil {
				t.Fatalf("plan.Build: %v", err)
			}

			// Context is deliberately bare. scaffold.runPlan calls
			// EnsurePlanCache internally (so the phase has a cache
			// object to Set/Get against) but the cache starts
			// empty for this invocation — no prior phase's
			// PlanCacheSet calls carry over. No
			// provisioning.SetSSHPool call either, so every
			// BorrowSSH falls back to a direct dial (the nil-pool
			// branch in common.BorrowSSH).
			t.Logf("phase %d/%d %q: applying (fresh plan, empty cache, no pool)",
				phaseIdx, len(phases), phase)
			err = ex.Execute(phaseCtx, scaffold.ExecuteOptions{
				Progress:   progress.Noop{},
				OnlyPhases: []string{phase},
			})
			if err != nil {
				t.Fatalf("phase %q failed in isolation (fresh BuildPlan, empty cache, no pool): %v",
					phase, err)
			}
			t.Logf("phase %d/%d %q: ok", phaseIdx, len(phases), phase)
		})
		if !ok {
			t.Fatalf("phase %d/%d %q failed; later phases depend on infrastructure state it was supposed to produce, halting",
				phaseIdx, len(phases), phase)
		}
	}

	t.Log("milestone: TestPhasesIsolatedNoCache PASSED — every phase converged cold")
}
