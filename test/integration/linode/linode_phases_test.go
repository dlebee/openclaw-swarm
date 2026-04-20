//go:build integration_linode

package linode

import (
	"context"
	"os"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// Per-phase timeout: generous enough that Linode API latency +
// apt/npm cold caches + Caddy/ACME on the mesh phases don't false-
// positive as a phase hang. The whole test is capped at 35 min by
// the outer context so a genuinely stuck phase still terminates
// within the CI envelope.
const linodePhasesPerPhaseTimeout = 10 * time.Minute

// TestPhasesIsolatedNoCache is the Linode counterpart to the
// Multipass tier's TestPhasesIsolatedNoCache. It applies each
// `claws apply` phase as a separate cold-start invocation against
// the same prefix: fresh BuildPlan, fresh Executable, fresh
// context.Context with only an empty EnsurePlanCache, no pre-seeded
// SSH pool, no cross-phase state re-hydration.
//
// This simulates an operator running
//
//	claws apply --only <phase>
//
// as a series of separate cold CLI invocations. The production CLI
// does pre-seed an SSH pool (see
// internal/cli/commands/apply/apply.go — SetSSHPool immediately
// after EnsurePlanCache), so this test is strictly COLDER than a
// realistic `apply --only` run. The nil-pool branch in
// common.BorrowSSH falls back to a direct dial so phases that use
// the pool keep working.
//
// What this test surfaces that TestCronAgentWithNodeExec does NOT:
// cross-phase plan-cache coupling. The production apply pipeline
// runs all phases in one process with a single shared plan cache,
// so a step in phase B that reads a key set by phase A works even
// though the step has no infrastructure-side check for the value.
// Under `apply --only B` from cold, that step fails. The intent of
// this test is to expose exactly those couplings before a
// production operator hits them — and to do it on a Linode tier
// where Caddy/ACME + real WireGuard add coverage the Multipass
// tier can't provide.
//
// Phases under test are enumerated at runtime via plan.PhaseNames()
// rather than hard-coded. The fixture intentionally carries
// machines + gateway + channels + nodes + agents so every
// conditional AddPhase call in apply.BuildPlan fires:
//
//   - provisioning  (always)
//   - security      (always)
//   - mesh-gateway  (headscale + sslip public_hostname)
//   - mesh-join
//   - gateway       (manifest.Gateways non-empty)
//   - channels      (gateway.channels non-empty — telegram-main)
//   - node          (manifest.Nodes non-empty)
//   - agents        (manifest.Agents non-empty)
//
// Abort-on-first-failure: once a phase fails, later phases can't
// meaningfully run, so t.FailNow halts the enclosing test. The
// first-failing phase name is the signal.
//
// Cleanup: a single t.Cleanup registered BEFORE any apply scans
// the prefix tag with provider.ListByTag and deletes every
// matching instance. Because Linode bills by the minute and the
// g6-standard-1 SKU is $0.018/hr, a leaked prefix costs ~$0.0003
// per minute per VM; the pre-apply cleanup registration is cheap
// insurance against a crash mid-test.
//
// Cost envelope: ~$0.005 per run (two g6-standard-1 × ~15 min,
// ~$0.018/hr). 35 min outer cap absorbs cold apt/npm caches plus
// Let's Encrypt rate-limit backoff on the first cert issuance.
func TestPhasesIsolatedNoCache(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer outerCancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
		data, err := os.ReadFile(privPath)
		if err == nil {
			saved := "/tmp/linode-phases-it-key"
			_ = os.WriteFile(saved, data, 0o600)
			t.Logf("CLAWS_IT_KEEP_VMS: private key saved to %s (use: ssh -i %s agent@<ip>)", saved, saved)
		}
	}

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-phases.yml")
	m.Prefix = "it-lin-phases-" + randSuffix(t)
	prefix := m.Prefix
	// Channel token — consumed by the `channels` phase via the
	// telegram plugin's LookupEnvFromManifest → env_file path.
	// See injectFakeTelegramTokens for why we route through an
	// env_file instead of t.Setenv (required to keep the test
	// parallel-safe with its siblings).
	injectFakeTelegramTokens(t, m, nil)

	// --- provider + SSH dialer ---------------------------------------------

	prov := linode.NewProvider(tok)
	dial := sshDialFunc(signer)

	// Cleanup runs once at the end regardless of which phase aborted.
	// Uses prov.ListByTag + DeleteInstance so a mid-phase failure
	// still tears everything down; no per-phase cleanup needed.
	t.Cleanup(func() {
		if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
			t.Logf("CLAWS_IT_KEEP_VMS set → leaving VMs up for debug (prefix=%s)", prefix)
			return
		}
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

	// --- discover phase list from a probe build -----------------------------
	//
	// Build the plan once up front just to enumerate PhaseNames().
	// We deliberately do NOT reuse this plan for execution: every
	// sub-test below rebuilds its own plan so each phase sees the
	// same "fresh BuildPlan + fresh Executable" contract. The probe
	// build is cheap (no API calls, no SSH — just target + step
	// wiring in memory).
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

			phaseCtx, cancel := context.WithTimeout(outerCtx, linodePhasesPerPhaseTimeout)
			defer cancel()

			// Fresh BuildPlan per sub-test. This re-runs
			// BuildMachineTargets, producing fresh MachineTarget
			// pointers with Instance == nil. The only cross-phase
			// state that survives is what's on the actual Linode
			// instances (discoverable via provider.ListByTag,
			// files and systemd units on disk). Everything the
			// production run kept in-memory on the plan cache —
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
