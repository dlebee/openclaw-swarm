//go:build integration_linode

package linode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// Fake bot tokens for parallel agents test
const (
	fakeTelegramParallelMainToken      = "000000003:FAKE_CLAWS_IT_LINODE_PARALLEL_MAIN_TOKEN_DO_NOT_XXX"
	fakeTelegramParallelSecondaryToken = "000000004:FAKE_CLAWS_IT_LINODE_PARALLEL_SECONDARY_TOKEN_DO_NOT_XXX"
)

// TestAgentsParallel tests that multiple agents can be created in parallel
// without race conditions. This validates the fix for the parallel agent
// creation issue in OpenClaw.
func TestAgentsParallel(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-agents-parallel.yml")
	m.Prefix = "it-lin-agents-par-" + randSuffix(t)
	prefix := m.Prefix

	// --- fake token plumbing ------------------------------------------------
	injectFakeTelegramTokens(t, m, map[string]string{
		"telegram-main":      fakeTelegramParallelMainToken,
		"telegram-secondary": fakeTelegramParallelSecondaryToken,
	})

	// --- fixture sanity checks ----------------------------------------------

	if len(m.Machines) != 1 {
		t.Fatalf("fixture sanity: expected 1 machine, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	if len(m.Agents) != 3 {
		t.Fatalf("fixture sanity: expected 3 agents for parallel test, got %d", len(m.Agents))
	}

	// Verify we have main, research, and ops agents
	agentIDs := make(map[string]bool)
	for _, ag := range m.Agents {
		agentIDs[ag.ID] = true
	}
	for _, want := range []string{"main", "research", "ops"} {
		if !agentIDs[want] {
			t.Fatalf("fixture sanity: missing agent %q", want)
		}
	}

	gw := &m.Gateways[0]
	if len(gw.Channels) != 2 {
		t.Fatalf("fixture sanity: expected 2 channels for parallel test, got %d", len(gw.Channels))
	}

	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
	}

	// --- apply authorization + provisioning ---------------------------------

	applySSHPubKey(t, m, pubKey)

	dial := provisioning.SSHDialWithSignerFunc(signer)
	inj := makeInjector(ctx, t, tok, dial)

	plan, err := planapply.BuildPlan(m, planapply.WithResolvers(inj))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	rep := progress.NewTestReporter(t)
	if err := scaffold.ApplyPlan(ctx, plan, rep); err != nil {
		t.Fatalf("apply plan: %v", err)
	}
	t.Cleanup(func() {
		linodeCleanup(t, tok, prefix)
	})

	// --- assertions ---------------------------------------------------------

	mc := m.Machines[0]

	ips, err := linode.GetInstanceIPs(ctx, tok, prefix+"-"+mc.Name)
	if err != nil {
		t.Fatalf("[%s] fetch ips: %v", mc.Name, err)
	}
	if ips.Public == nil {
		t.Fatalf("[%s] no public ip assigned", mc.Name)
	}
	host := ips.Public.String()

	cfg := fetchOpenclawConfig(t, dial, host, mc)

	// Assert all three agents exist in the config
	t.Log("Checking all agents were created...")
	for _, ag := range m.Agents {
		wantWorkspace := agentWorkspaceForAssertions(ag)
		assertAgentInConfig(t, mc.Name, cfg, ag.ID, ag.Model.Primary, wantWorkspace)
		t.Logf("[%s] agent %q present with model=%q workspace=%q",
			mc.Name, ag.ID, ag.Model.Primary, wantWorkspace)
	}

	// Verify agent count matches expected
	if len(cfg.Agents.List) != len(m.Agents) {
		t.Errorf("[%s] agents.list has %d entries, want %d",
			mc.Name, len(cfg.Agents.List), len(m.Agents))
	}

	// Verify both telegram channels are configured
	if len(cfg.Channels.Telegram.Accounts) != 2 {
		t.Errorf("[%s] expected 2 telegram accounts, got %d",
			mc.Name, len(cfg.Channels.Telegram.Accounts))
	}

	// Verify tools.elevated is configured for all agents
	if cfg.Tools.Elevated.Enabled == nil || !*cfg.Tools.Elevated.Enabled {
		t.Errorf("[%s] tools.elevated.enabled should be true", mc.Name)
	}

	// Verify workspace files exist for secondary agents
	for _, ag := range m.Agents {
		if ag.ID == "main" {
			continue // main uses default workspace, already covered
		}
		workspace := agentWorkspaceForAssertions(ag)
		if workspace == "" {
			continue
		}
		// Check SOUL.md exists
		assertWorkspaceFileContains(t, dial, host, mc, workspace, "SOUL.md", "You are")
		// Check AGENTS.md exists
		assertWorkspaceFileContains(t, dial, host, mc, workspace, "AGENTS.md", "You are")
	}

	t.Logf("[%s] parallel agents test passed: all %d agents created successfully",
		mc.Name, len(m.Agents))
}

// fetchOpenclawConfig reads and parses ~/.openclaw/openclaw.json from the
// remote machine. This is a copy of the helper from linode_agents_test.go
// since we can't easily share helpers across build-tagged files.
func fetchOpenclawConfig(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) agentsPhaseConfig {
	t.Helper()
	configPath := "~/.openclaw/openclaw.json"
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := dial(ctx, host, 22, user)
	if err != nil {
		t.Fatalf("[%s] dial %s@%s for sftp: %v", mc.Name, user, host, err)
	}
	defer client.Close()

	raw, err := sshfile.ReadFile(client, configPath)
	if err != nil {
		t.Fatalf("[%s] sftp read %s: %v", mc.Name, configPath, err)
	}

	var cfg agentsPhaseConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		snippet := string(raw)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "… (truncated)"
		}
		t.Fatalf("[%s] decode openclaw.json: %v\nraw:\n%s", mc.Name, err, snippet)
	}
	t.Logf("[%s] fetched openclaw.json (%d bytes) via sftp", mc.Name, len(raw))
	return cfg
}
