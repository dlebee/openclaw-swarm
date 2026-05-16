//go:build integration_linode

package linode

import (
	"context"
	"encoding/json"
	"net"
	"path"
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

// Fake bot token, deliberately DIFFERENT from every other token
// constant in the test tree (both tiers) so a grep for the
// literal value in log/CI output points directly at the exact
// test file that planted it. Format matches Telegram's shape so a
// future string-level sanity pass in openclaw's setup adapter
// can't reject it on syntactic grounds.
const fakeTelegramAgentsMainToken = "000000002:FAKE_CLAWS_IT_LINODE_AGENTS_MAIN_TOKEN_DO_NOT_XXX"

// agentsPhaseConfig: narrow slice of ~/.openclaw/openclaw.json the
// Linode agents test needs. Intentionally duplicated with the
// Multipass tier's equivalent struct (multipass_agents_test.go) —
// shared integration-test helpers are a maintenance trap (build
// tags differ, package-local helpers differ, once you start
// sharing you're building a whole testing-sibling package). Cheap
// to duplicate; re-evaluate if we add a third provider tier.
type agentsPhaseConfig struct {
	Agents struct {
		List []configAgent `json:"list"`
	} `json:"agents"`
	Tools struct {
		Elevated configElevated `json:"elevated"`
	} `json:"tools"`
	Channels struct {
		Telegram struct {
			Accounts map[string]configTelegramAccount `json:"accounts"`
		} `json:"telegram"`
	} `json:"channels"`
}

type configAgent struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"`
}

// configElevated matches the shape `openclaw config get tools.
// elevated --json` returns, same as ConfigureToolsStep.
// readElevatedConfig (configure_tools.go:215) parses internally.
type configElevated struct {
	Enabled   *bool               `json:"enabled,omitempty"`
	AllowFrom map[string][]string `json:"allowFrom,omitempty"`
}

type configTelegramAccount struct {
	BotToken string `json:"botToken"`
}

// cliBinding matches `openclaw agents bindings --agent <id> --json`.
// The CLI emits nested {agentId, match:{channel, accountId}} — same
// shape as the in-tree BindingInfo (service.go:77).
type cliBinding struct {
	AgentID string `json:"agentId"`
	Match   struct {
		Channel string `json:"channel"`
		Account string `json:"accountId"`
	} `json:"match"`
}

func (b cliBinding) Channel() string { return b.Match.Channel }
func (b cliBinding) Account() string { return b.Match.Account }

// cliAgent matches `openclaw agents list --json` — same shape as
// the in-tree AgentInfo (service.go:13).
type cliAgent struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"`
}

// TestAgentsSmoke is the Linode counterpart to the Multipass tier's
// TestAgentsSmoke. It runs provisioning + security + gateway +
// channels + agents on a single-instance, single-agent manifest
// against real Linode hardware and asserts the full agents phase
// (add-agent, ensure-model, configure-workspace, configure-tools,
// configure-bindings) lands config + workspace-file state
// correctly.
//
// Why this Linode mirror is worth the dollar-pennies cost over
// the Multipass tier's identical assertions:
//
//   - `openclaw agents add` / `openclaw agents bind` / `openclaw
//     config set` resolve against the real public npm-installed
//     openclaw package on a real Linode ubuntu24.04 image. A
//     regression in how the bundled agents CLI finds its
//     extensions when globally npm-installed would bite here
//     first. (Multipass uses the same apt mirror chain but a
//     different base image; /usr/lib/node_modules path resolution
//     has shown subtle differences before.)
//   - Loopback bind invariant under agents-phase activity on a
//     public-IP VM. Provisioning + security + gateway + channels
//     on Linode already prove the gateway binds 127.0.0.1:18789;
//     this test re-asserts AFTER the agents phase runs, so a
//     future regression where agents phase config writes flip
//     gateway.bind to "lan" would be caught before reaching a
//     real public gateway.
//   - `openclaw agents set-identity` + managed-section markers in
//     SOUL.md / AGENTS.md / IDENTITY.md, written by configure-
//     workspace, on a real cloud filesystem. Inotify edge cases
//     between the hypervisor storage and the TypeScript CLI's
//     file-watch / atomic-write code occasionally manifest
//     differently than on Multipass's shared-kernel bridge.
//
// Fake token is safe on Linode for the exact same reason it's
// safe on Multipass: apply-time asserts happen between SSH dial
// and config-file write; Telegram's API is never hit during the
// phases under test, only later by the long-running daemon, which
// 401s in the background without affecting gateway health.
//
// Phase frontier: provisioning, security, gateway, channels,
// agents. Mesh, node not declared in the fixture — covered by
// TestMeshSmoke and TestNodeSmoke respectively. A test that
// combines agents + node + tools.exec is a natural follow-up once
// this smoke is green.
//
// Cost envelope: ~12 min on one g6-standard-1 at us-east ≈
// $0.003. 25min cap absorbs cold apt mirrors, npm registry
// latency, and the usual Linode provisioning jitter (especially
// the first-boot uptime before sshd opens port 22).
func TestAgentsSmoke(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-agents.yml")
	m.Prefix = "it-lin-agents-" + randSuffix(t)
	prefix := m.Prefix
	// --- fake token plumbing ------------------------------------------------
	//
	// Parallel-safe env-var injection via env_file. See
	// injectFakeTelegramTokens for why we can't use t.Setenv
	// inside a t.Parallel test.
	injectFakeTelegramTokens(t, m, map[string]string{
		"telegram-main": fakeTelegramAgentsMainToken,
	})

	if len(m.Machines) != 1 {
		t.Fatalf("fixture sanity: expected 1 machine, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	if len(m.Nodes) != 0 {
		t.Fatalf("fixture sanity: expected 0 nodes (agents smoke doesn't need a node), got %d",
			len(m.Nodes))
	}
	if len(m.Agents) != 1 {
		t.Fatalf("fixture sanity: expected 1 agent (single-agent surface is core), got %d",
			len(m.Agents))
	}
	gw := &m.Gateways[0]
	if gw.Name != "gateway" {
		t.Fatalf("fixture sanity: expected gateway name \"gateway\", got %q", gw.Name)
	}
	if gw.Networking != nil {
		t.Fatalf("fixture sanity: gateway %q must not have a networking block (loopback bind only), got %+v",
			gw.Name, gw.Networking)
	}
	if len(gw.Channels) != 1 {
		t.Fatalf("fixture sanity: expected 1 channel (agent needs something to bind to), got %d",
			len(gw.Channels))
	}
	if gw.Channels[0].Kind != manifestdata.ChannelKindTelegram {
		t.Fatalf("fixture sanity: channel %q kind = %q, want %q",
			gw.Channels[0].Name, gw.Channels[0].Kind, manifestdata.ChannelKindTelegram)
	}

	ag := &m.Agents[0]
	if ag.ID != "main" {
		t.Fatalf("fixture sanity: expected agent id \"main\", got %q", ag.ID)
	}
	if ag.Gateway != gw.Name {
		t.Fatalf("fixture sanity: agent %q gateway = %q, want %q", ag.ID, ag.Gateway, gw.Name)
	}
	if ag.Workspace == "" || ag.Model.Primary == "" {
		t.Fatalf("fixture sanity: agent %q has empty workspace or model.primary", ag.ID)
	}
	// Tools.Elevated presence is what makes ConfigureToolsStep.
	// Applicable return true. Drifting to nil silently skips the
	// step — fail loud.
	if ag.Tools == nil || ag.Tools.Elevated == nil {
		t.Fatalf("fixture sanity: agent %q must declare tools.elevated to exercise ConfigureToolsStep; got tools=%+v",
			ag.ID, ag.Tools)
	}
	if ag.Tools.Exec != nil {
		t.Fatalf("fixture sanity: agent %q must NOT declare tools.exec (needs a node, covered by follow-up); got %+v",
			ag.ID, ag.Tools.Exec)
	}
	if ag.Identity == nil || ag.Identity.Name == "" {
		t.Fatalf("fixture sanity: agent %q must declare identity.name", ag.ID)
	}
	if ag.Soul == "" || ag.AgentsMD == "" {
		t.Fatalf("fixture sanity: agent %q must declare soul + agents_md", ag.ID)
	}
	if len(ag.Bindings) != 1 {
		t.Fatalf("fixture sanity: expected exactly 1 binding, got %d", len(ag.Bindings))
	}
	if ag.Bindings[0].Channel != "telegram" || ag.Bindings[0].Account != "telegram-main" {
		t.Fatalf("fixture sanity: binding[0] = {channel:%q, account:%q}, want {telegram, telegram-main}",
			ag.Bindings[0].Channel, ag.Bindings[0].Account)
	}

	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
		}
		// Same guard as every other Linode test: agent_user ==
		// root collapses the systemd --user + linger coverage.
		if mc.AgentUser == "root" {
			t.Fatalf("fixture sanity: machine %q agent_user is 'root' — "+
				"TestAgentsSmoke needs a dedicated agent account to "+
				"exercise the systemd --user + linger path", mc.Name)
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

	for _, want := range []string{"provisioning", "security", "gateway", "channels", "agents"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + gateway + channels + agents on 1 Linode instance")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning", "security", "gateway", "channels", "agents"},
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

	// ---- gateway-still-healthy cross-check ------------------------------------
	//
	// Run these BEFORE the agents-specific assertions so a
	// regression where agents-phase config writes destabilize the
	// daemon shows up as "systemd inactive" / "port not listening"
	// — a clear pointer to the agents→gateway interaction — rather
	// than as a noisy downstream agents assertion failure.
	//
	// The loopback re-check on Linode is the single most valuable
	// assertion here: a public-IP VM running openclaw-gateway with
	// agents + channels configured must keep binding 127.0.0.1:
	// 18789. Any regression that flips the bind during an agents-
	// phase-triggered reload would be caught here before reaching
	// a production gateway on a real public IP.
	assertGatewayUnitActive(t, dial, host, mc)
	assertGatewayPortListening(t, dial, host, mc)

	// ---- agents-phase outside-in assertions -----------------------------------

	// 1. `openclaw agents list --json` reports the agent, with the
	//    manifest model. Canonical "did add-agent + ensure-model
	//    both land?" signal, and matches the surface production
	//    operators use to verify agent registration.
	assertAgentRegistered(t, dial, host, mc, ag.ID, ag.Model.Primary)

	// 2. `openclaw.json` on disk has the agent entry with matching
	//    scalars. Typed JSON-decode is stronger than the CLI check
	//    above — a regression that writes to a legacy path (e.g.
	//    agents.registered[]) leaves the struct's List empty and
	//    the loud failure message pinpoints the shape mismatch.
	cfg := fetchAgentsPhaseConfig(t, dial, host, mc)
	assertAgentInConfig(t, mc.Name, cfg, ag.ID, ag.Model.Primary, ag.Workspace)

	// 3. tools.elevated.enabled + allowFrom round-tripped end-to-
	//    end. Struct-decode (not plain `config get` scalar) proves
	//    the nested map also survived the config-set write.
	if cfg.Tools.Elevated.Enabled == nil || !*cfg.Tools.Elevated.Enabled {
		t.Errorf("[%s] tools.elevated.enabled = %v, want true", mc.Name, cfg.Tools.Elevated.Enabled)
	}
	wantAllow := ag.Tools.Elevated.AllowFrom
	for ch, wantIDs := range wantAllow {
		haveIDs := cfg.Tools.Elevated.AllowFrom[ch]
		for _, wid := range wantIDs {
			found := false
			for _, hid := range haveIDs {
				if hid == wid {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("[%s] tools.elevated.allowFrom[%q] missing %q; got %v",
					mc.Name, ch, wid, haveIDs)
			}
		}
	}

	// 4. channels.telegram.accounts.telegram-main still has the
	//    fake token (unredacted, via SFTP). Proves channels phase
	//    ran to completion; bindings depend on this account
	//    existing, so a missing token here would also poison
	//    `agents bind`.
	if acc, ok := cfg.Channels.Telegram.Accounts["telegram-main"]; !ok {
		keys := make([]string, 0, len(cfg.Channels.Telegram.Accounts))
		for k := range cfg.Channels.Telegram.Accounts {
			keys = append(keys, k)
		}
		t.Errorf("[%s] channels.telegram.accounts[telegram-main] missing; got keys=%v",
			mc.Name, keys)
	} else if acc.BotToken != fakeTelegramAgentsMainToken {
		t.Errorf("[%s] channels.telegram.accounts[telegram-main].botToken = %q, want %q",
			mc.Name, acc.BotToken, fakeTelegramAgentsMainToken)
	}

	// 5. SOUL.md, AGENTS.md, IDENTITY.md on disk in the workspace
	//    dir with CLAWS MANAGED markers + manifest content. The
	//    daemon doesn't consume these files, but any future
	//    feature (soul-drift detector, identity rotator, etc.)
	//    will — "bytes on disk match manifest" is the contract.
	assertWorkspaceManagedFile(t, dial, host, mc, ag.Workspace, "SOUL.md", ag.Soul)
	assertWorkspaceManagedFile(t, dial, host, mc, ag.Workspace, "AGENTS.md", string(ag.AgentsMD))
	// IDENTITY.md body is synthesized by buildIdentityMD (configure_
	// workspace.go:63); assert on the name: / emoji: lines rather
	// than the full body so a cosmetic header change in that
	// builder doesn't force a test rewrite.
	assertWorkspaceFileContains(t, dial, host, mc, ag.Workspace, "IDENTITY.md",
		"name: "+ag.Identity.Name)
	assertWorkspaceFileContains(t, dial, host, mc, ag.Workspace, "IDENTITY.md",
		"emoji: "+ag.Identity.Emoji)

	// 6. `openclaw agents bindings --agent main --json` returns
	//    the (telegram, telegram-main) tuple. Mirrors the step's
	//    own Verify surface (configure_bindings.go:109).
	assertAgentBinding(t, dial, host, mc, ag.ID, "telegram", "telegram-main")

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
	if after := waitForEmptyListByTag(ctx, t, prov, "claws/"+prefix, 30*time.Second); len(after) != 0 {
		t.Errorf("ListByTag after destroy still returned %d instances: %+v", len(after), after)
	}
}

// ---------------------------------------------------------------------------
// agents-phase assertion helpers (Linode-local)
//
// Duplicated with the Multipass tier for the same reason every
// other helper family in this package duplicates — build-tag
// boundary, test-local conveniences, no third tier yet to justify
// a shared sub-package.
// ---------------------------------------------------------------------------

func assertAgentRegistered(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, wantID, wantModel string) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, `openclaw agents list --json 2>/dev/null`)
	if err != nil {
		t.Errorf("[%s] openclaw agents list --json: %v\n%s", mc.Name, err, out)
		return
	}
	idx := strings.IndexByte(out, '[')
	if idx < 0 {
		t.Errorf("[%s] agents list output had no JSON array:\n%s", mc.Name, out)
		return
	}
	raw := out[idx:]
	var agents []cliAgent
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		t.Errorf("[%s] parse agents list: %v\nraw:\n%s", mc.Name, err, raw)
		return
	}
	for _, a := range agents {
		if a.ID == wantID {
			if a.Model != wantModel {
				t.Errorf("[%s] agent %q model = %q, want %q",
					mc.Name, wantID, a.Model, wantModel)
			}
			t.Logf("[%s] agents list: %s/%s registered (workspace=%s)",
				mc.Name, a.ID, a.Model, a.Workspace)
			return
		}
	}
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.ID)
	}
	t.Errorf("[%s] agent %q not in agents list; got ids=%v (full raw: %s)",
		mc.Name, wantID, ids, out)
}

func assertAgentBinding(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, agentID, wantChannel, wantAccount string) {
	t.Helper()
	cmd := `openclaw agents bindings --agent ` + shellQuote(agentID) + ` --json 2>/dev/null`
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, cmd)
	if err != nil {
		t.Errorf("[%s] openclaw agents bindings --agent %q: %v\n%s", mc.Name, agentID, err, out)
		return
	}
	idx := strings.IndexByte(out, '[')
	if idx < 0 {
		t.Errorf("[%s] agents bindings output had no JSON array:\n%s", mc.Name, out)
		return
	}
	raw := out[idx:]
	var bindings []cliBinding
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		t.Errorf("[%s] parse agents bindings: %v\nraw:\n%s", mc.Name, err, raw)
		return
	}
	for _, b := range bindings {
		if b.Channel() == wantChannel && b.Account() == wantAccount {
			t.Logf("[%s] agents bindings: %s → %s:%s", mc.Name, agentID, b.Channel(), b.Account())
			return
		}
	}
	t.Errorf("[%s] binding %s:%s not found for agent %q; got bindings=%+v",
		mc.Name, wantChannel, wantAccount, agentID, bindings)
}

// shellQuote wraps s in single quotes for safe use inside `bash
// -c` / `ssh ... 'CMD'`. Defensive — agent IDs in this test are
// bare identifiers but we don't want this helper to be a
// liability if richer IDs show up.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fetchAgentsPhaseConfig(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) agentsPhaseConfig {
	t.Helper()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	configPath := path.Join("/home", user, ".openclaw", "openclaw.json")

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

func assertAgentInConfig(t *testing.T, machineName string, cfg agentsPhaseConfig, wantID, wantModel, wantWorkspace string) {
	t.Helper()
	for _, a := range cfg.Agents.List {
		if a.ID == wantID {
			if a.Model != wantModel {
				t.Errorf("[%s] agents.list entry for %q: model = %q, want %q",
					machineName, wantID, a.Model, wantModel)
			}
			if a.Workspace == "" {
				t.Errorf("[%s] agents.list entry for %q: workspace is empty", machineName, wantID)
			}
			// Accept either "~/.openclaw/workspace" or the
			// resolved "/home/<user>/.openclaw/workspace" —
			// whichever the CLI chose to persist.
			suffix := strings.TrimPrefix(wantWorkspace, "~")
			if !strings.HasSuffix(a.Workspace, suffix) {
				t.Errorf("[%s] agents.list entry for %q: workspace = %q, want suffix %q",
					machineName, wantID, a.Workspace, suffix)
			}
			return
		}
	}
	ids := make([]string, 0, len(cfg.Agents.List))
	for _, a := range cfg.Agents.List {
		ids = append(ids, a.ID)
	}
	t.Errorf("[%s] agents.list missing entry for %q; got ids=%v", machineName, wantID, ids)
}

// resolveWorkspaceDir mirrors the in-tree resolveWorkspace
// (configure_workspace.go:76) but runs in the test harness using
// the same /home/<agent> hardcoded mapping we already rely on for
// the config file. If a future fixture uses a non-default HOME
// this needs updating in lockstep with every sftp helper.
func resolveWorkspaceDir(mc manifestdata.Machine, workspace string) string {
	if !strings.HasPrefix(workspace, "~/") {
		return workspace
	}
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	return path.Join("/home", user, workspace[2:])
}

func assertWorkspaceManagedFile(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, workspace, filename, wantContent string) {
	t.Helper()
	dir := resolveWorkspaceDir(mc, workspace)
	fullPath := path.Join(dir, filename)

	user := mc.AgentUser
	if user == "" {
		user = "root"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := dial(ctx, host, 22, user)
	if err != nil {
		t.Errorf("[%s] dial %s@%s for sftp: %v", mc.Name, user, host, err)
		return
	}
	defer client.Close()

	raw, err := sshfile.ReadFile(client, fullPath)
	if err != nil {
		t.Errorf("[%s] sftp read %s: %v", mc.Name, fullPath, err)
		return
	}
	text := string(raw)

	if !strings.Contains(text, "<!-- CLAWS MANAGED START") {
		t.Errorf("[%s] %s: missing CLAWS MANAGED START marker", mc.Name, filename)
	}
	if !strings.Contains(text, "<!-- CLAWS MANAGED END") {
		t.Errorf("[%s] %s: missing CLAWS MANAGED END marker", mc.Name, filename)
	}
	trimmed := strings.TrimSpace(wantContent)
	if !strings.Contains(text, trimmed) {
		snippet := text
		if len(snippet) > 512 {
			snippet = snippet[:512] + "… (truncated)"
		}
		t.Errorf("[%s] %s: missing expected content substring %q; got:\n%s",
			mc.Name, filename, firstLine(trimmed), snippet)
	} else {
		t.Logf("[%s] %s: managed section intact (%d bytes)", mc.Name, filename, len(raw))
	}
}

func assertWorkspaceFileContains(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, workspace, filename, want string) {
	t.Helper()
	dir := resolveWorkspaceDir(mc, workspace)
	fullPath := path.Join(dir, filename)

	user := mc.AgentUser
	if user == "" {
		user = "root"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := dial(ctx, host, 22, user)
	if err != nil {
		t.Errorf("[%s] dial %s@%s for sftp: %v", mc.Name, user, host, err)
		return
	}
	defer client.Close()

	raw, err := sshfile.ReadFile(client, fullPath)
	if err != nil {
		t.Errorf("[%s] sftp read %s: %v", mc.Name, fullPath, err)
		return
	}
	if !strings.Contains(string(raw), want) {
		t.Errorf("[%s] %s: missing expected substring %q", mc.Name, filename, want)
	} else {
		t.Logf("[%s] %s: contains %q", mc.Name, filename, want)
	}
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
