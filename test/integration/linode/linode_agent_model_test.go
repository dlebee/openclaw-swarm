//go:build integration_linode

package linode

import (
	"context"
	"encoding/json"
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

// Fake bot token for the agent model test.
const fakeTelegramAgentModelToken = "000000003:FAKE_CLAWS_IT_LINODE_AGENT_MODEL_TOKEN_DO_NOT_X"

// TestAgentModelSwap verifies that the ensure-model step correctly
// detects and repairs drift in both primary model AND fallbacks.
//
// Test flow:
//  1. Apply with model=opus, fallbacks=[sonnet]
//  2. Assert model config landed correctly
//  3. Change manifest to model=sonnet, fallbacks=[opus] (swapped)
//  4. Re-apply (agents phase only for speed)
//  5. Assert model config was updated
//
// This catches the bug where Check only compared primary model,
// ignoring fallbacks, causing the step to skip when only fallbacks changed.
//
// Cost envelope: ~12 min on one g6-standard-1 ≈ $0.003.
func TestAgentModelSwap(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-agent-model.yml")
	m.Prefix = "it-lin-mdl-" + randSuffix(t)
	prefix := m.Prefix

	injectFakeTelegramTokens(t, m, map[string]string{
		"telegram-main": fakeTelegramAgentModelToken,
	})

	if len(m.Machines) != 1 {
		t.Fatalf("fixture sanity: expected 1 machine, got %d", len(m.Machines))
	}
	if len(m.Agents) != 1 {
		t.Fatalf("fixture sanity: expected 1 agent, got %d", len(m.Agents))
	}

	ag := &m.Agents[0]
	if ag.Model.Primary == "" {
		t.Fatalf("fixture sanity: agent %q has empty model.primary", ag.ID)
	}
	if len(ag.Model.Fallbacks) == 0 {
		t.Fatalf("fixture sanity: agent %q has no fallbacks (need at least 1 for swap test)", ag.ID)
	}

	initialPrimary := ag.Model.Primary
	initialFallbacks := make([]string, len(ag.Model.Fallbacks))
	copy(initialFallbacks, ag.Model.Fallbacks)

	t.Logf("initial config: primary=%q fallbacks=%v", initialPrimary, initialFallbacks)

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
			t.Logf("cleanup: deleting %s", inst.Label)
			if err := prov.DeleteInstance(cleanupCtx, inst.ResourceID); err != nil {
				t.Logf("cleanup: delete %s: %v", inst.Label, err)
			}
		}
	})

	// --- first apply: initial model config ---------------------------------

	plan, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:  m,
		Provider:  prov,
		SSHPubKey: pubKey,
		SSHDial:   dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("PHASE 1: applying full plan with initial model config")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning", "security", "gateway", "channels", "agents"},
	}); err != nil {
		t.Fatalf("execute plan (phase 1): %v", err)
	}

	// --- verify initial state ----------------------------------------------

	mc := m.Machines[0]
	mt := findMachineTarget(t, plan, mc.Name)
	if mt.Instance == nil {
		t.Fatalf("machine %q has no Instance after apply", mc.Name)
	}
	inst := mt.Instance
	if strings.TrimSpace(inst.PublicIPv4) == "" || net.ParseIP(inst.PublicIPv4) == nil {
		t.Fatalf("machine %q PublicIPv4 %q is not a valid IP", mc.Name, inst.PublicIPv4)
	}
	host := inst.PublicIPv4

	t.Logf("machine %q → ip=%s", mc.Name, host)

	// Verify initial model landed
	model, fallbacks := fetchAgentModelConfig(t, dial, host, mc, ag.ID)
	if model != initialPrimary {
		t.Errorf("initial primary model = %q, want %q", model, initialPrimary)
	}
	if !stringSlicesEqual(fallbacks, initialFallbacks) {
		t.Errorf("initial fallbacks = %v, want %v", fallbacks, initialFallbacks)
	}
	t.Logf("verified initial config: primary=%q fallbacks=%v", model, fallbacks)

	// Verify the per-model runtime pin landed on the primary only.
	initialPin := ag.Models[initialPrimary].Runtime
	if initialPin == "" {
		t.Fatalf("fixture sanity: agent %q pins no runtime for primary %q", ag.ID, initialPrimary)
	}
	runtimes := fetchAgentModelRuntimes(t, dial, host, mc, ag.ID)
	if runtimes[initialPrimary] != initialPin {
		t.Errorf("initial runtime for %q = %q, want %q", initialPrimary, runtimes[initialPrimary], initialPin)
	}
	if got := runtimes[initialFallbacks[0]]; got != "" {
		t.Errorf("fallback %q runtime = %q, want unpinned", initialFallbacks[0], got)
	}
	t.Logf("verified initial runtime pins: %v", runtimes)

	// --- swap model and fallbacks ------------------------------------------

	// Swap: primary becomes first fallback, first fallback becomes primary
	swappedPrimary := initialFallbacks[0]
	swappedFallbacks := []string{initialPrimary}
	if len(initialFallbacks) > 1 {
		swappedFallbacks = append(swappedFallbacks, initialFallbacks[1:]...)
	}

	t.Logf("swapping to: primary=%q fallbacks=%v", swappedPrimary, swappedFallbacks)

	ag.Model.Primary = swappedPrimary
	ag.Model.Fallbacks = swappedFallbacks
	// Move the runtime pin to the new primary. The old pin is deliberately
	// left in the manifest's shadow: claws is additive on the models map,
	// so dropping a ref here must NOT unpin it on the gateway.
	ag.Models = manifestdata.AgentModels{swappedPrimary: {Runtime: initialPin}}

	// --- second apply: only agents phase -----------------------------------

	plan2, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:  m,
		Provider:  prov,
		SSHPubKey: pubKey,
		SSHDial:   dial,
	})
	if err != nil {
		t.Fatalf("build plan (phase 2): %v", err)
	}

	ex2, err := plan2.Build()
	if err != nil {
		t.Fatalf("plan.Build (phase 2): %v", err)
	}

	t.Log("PHASE 2: re-applying agents phase only with swapped model config")
	if err := ex2.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"agents"},
	}); err != nil {
		t.Fatalf("execute plan (phase 2): %v", err)
	}

	// --- verify swapped state ----------------------------------------------

	model2, fallbacks2 := fetchAgentModelConfig(t, dial, host, mc, ag.ID)
	if model2 != swappedPrimary {
		t.Errorf("swapped primary model = %q, want %q", model2, swappedPrimary)
	}
	if !stringSlicesEqual(fallbacks2, swappedFallbacks) {
		t.Errorf("swapped fallbacks = %v, want %v", fallbacks2, swappedFallbacks)
	}
	t.Logf("verified swapped config: primary=%q fallbacks=%v", model2, fallbacks2)

	runtimes2 := fetchAgentModelRuntimes(t, dial, host, mc, ag.ID)
	if runtimes2[swappedPrimary] != initialPin {
		t.Errorf("swapped runtime for %q = %q, want %q", swappedPrimary, runtimes2[swappedPrimary], initialPin)
	}
	if runtimes2[initialPrimary] != initialPin {
		t.Errorf("runtime for %q = %q, want %q preserved (claws is additive on the models map)",
			initialPrimary, runtimes2[initialPrimary], initialPin)
	}
	t.Logf("verified swapped runtime pins: %v", runtimes2)

	// --- teardown ----------------------------------------------------------

	insts, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after apply: %v", err)
	}
	for _, inst := range insts {
		if err := prov.DeleteInstance(ctx, inst.ResourceID); err != nil {
			t.Errorf("DeleteInstance %s: %v", inst.Label, err)
		}
	}
	if after := waitForEmptyListByTag(ctx, t, prov, "claws/"+prefix, 30*time.Second); len(after) != 0 {
		t.Errorf("ListByTag after destroy still returned %d instances: %+v", len(after), after)
	}
}

// fetchAgentModelConfig retrieves the model primary and fallbacks from
// the remote openclaw config via `openclaw config get`.
func fetchAgentModelConfig(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, agentID string) (primary string, fallbacks []string) {
	t.Helper()

	// Get the raw model config - can be string or object
	cmd := `openclaw config get agents.list --json 2>/dev/null`
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, cmd)
	if err != nil {
		t.Fatalf("[%s] openclaw config get: %v\n%s", mc.Name, err, out)
	}

	// Find JSON start
	idx := strings.Index(out, "[")
	if idx < 0 {
		t.Fatalf("[%s] config get output had no JSON array:\n%s", mc.Name, out)
	}
	raw := out[idx:]

	// Parse agents list
	var agents []struct {
		ID    string          `json:"id"`
		Model json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		t.Fatalf("[%s] parse agents list: %v\nraw:\n%s", mc.Name, err, raw)
	}

	// Find our agent
	for _, a := range agents {
		if strings.EqualFold(a.ID, agentID) {
			// Try object form first
			var modelObj struct {
				Primary   string   `json:"primary"`
				Fallbacks []string `json:"fallbacks"`
			}
			if err := json.Unmarshal(a.Model, &modelObj); err == nil && modelObj.Primary != "" {
				return modelObj.Primary, modelObj.Fallbacks
			}
			// Try string form
			var modelStr string
			if err := json.Unmarshal(a.Model, &modelStr); err == nil {
				return modelStr, nil
			}
			t.Fatalf("[%s] could not parse model for agent %q: %s", mc.Name, agentID, string(a.Model))
		}
	}

	t.Fatalf("[%s] agent %q not found in config", mc.Name, agentID)
	return "", nil
}

// stringSlicesEqual compares two string slices for equality.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchAgentModelRuntimes retrieves the per-model runtime pins
// (agents.list[].models[<ref>].agentRuntime.id) for one agent, keyed by
// model ref. Refs with no pin are absent from the returned map.
func fetchAgentModelRuntimes(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, agentID string) map[string]string {
	t.Helper()

	cmd := `openclaw config get agents.list --json 2>/dev/null`
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, cmd)
	if err != nil {
		t.Fatalf("[%s] openclaw config get: %v\n%s", mc.Name, err, out)
	}

	idx := strings.Index(out, "[")
	if idx < 0 {
		t.Fatalf("[%s] config get output had no JSON array:\n%s", mc.Name, out)
	}
	raw := out[idx:]

	var agents []struct {
		ID     string `json:"id"`
		Models map[string]struct {
			AgentRuntime struct {
				ID string `json:"id"`
			} `json:"agentRuntime"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		t.Fatalf("[%s] parse agents list: %v\nraw:\n%s", mc.Name, err, raw)
	}

	pins := map[string]string{}
	for _, a := range agents {
		if !strings.EqualFold(a.ID, agentID) {
			continue
		}
		for ref, entry := range a.Models {
			if entry.AgentRuntime.ID != "" {
				pins[ref] = entry.AgentRuntime.ID
			}
		}
		return pins
	}
	t.Fatalf("[%s] agent %q not found in config", mc.Name, agentID)
	return nil
}
