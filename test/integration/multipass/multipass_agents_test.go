//go:build integration_multipass

package multipass

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// Fake bot token for the single telegram channel the agent binds to.
// Same "daemon 401s silently against api.telegram.org, gateway stays
// healthy" argument as TestChannelsSmoke — the agents phase only
// writes config, never reaches the network. We use a value that is
// deliberately NOT shared with TestChannelsSmoke so a leak in logs
// points back to the exact test that planted it.
const fakeTelegramAgentsMainToken = "000000000:FAKE_CLAWS_IT_AGENTS_MAIN_TOKEN_DO_NOT_USE_XX"

// agentsPhaseConfig is the narrow slice of ~/.openclaw/openclaw.json
// this test cares about. Keeping the struct narrow means a schema
// migration that adds or reorders unrelated keys won't break the
// test — only changes to agents.list / tools.elevated / channels.
// telegram.accounts matter. A regression that relocates agent
// entries (e.g. agents.list → gateway.agents.list) would leave
// Agents empty and fail the presence check loudly instead of
// silently.
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

// configAgent captures the fields on agents.list[i] that the agents
// phase actually writes. Identity + bindings shapes aren't
// guaranteed to be under this path (the openclaw CLI stores them
// via `agents set-identity` and `agents bind`, which may land in a
// sibling subtree) — so we DON'T decode them here. The test uses
// the `openclaw agents list --json` / `agents bindings --json` CLIs
// for those assertions instead; those are stable public interfaces
// even if the on-disk layout changes.
type configAgent struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"`
}

// configElevated matches the shape `openclaw config get tools.
// elevated --json` returns, which is what ConfigureToolsStep's
// readElevatedConfig uses internally (see configure_tools.go:215).
// Using the same shape means a future schema change in openclaw's
// elevated config would break the step AND this test together,
// which is exactly what we want — a silent test would be worse
// than a loud one.
type configElevated struct {
	Enabled   *bool               `json:"enabled,omitempty"`
	AllowFrom map[string][]string `json:"allowFrom,omitempty"`
}

type configTelegramAccount struct {
	BotToken string `json:"botToken"`
}

// cliBinding matches `openclaw agents bindings --agent <id> --json`
// output. The CLI emits nested {agentId, match:{channel, accountId}},
// same shape as the in-tree BindingInfo struct (service.go:77).
type cliBinding struct {
	AgentID string `json:"agentId"`
	Match   struct {
		Channel string `json:"channel"`
		Account string `json:"accountId"`
	} `json:"match"`
}

func (b cliBinding) Channel() string { return b.Match.Channel }
func (b cliBinding) Account() string { return b.Match.Account }

// cliAgentList matches `openclaw agents list --json` output, same
// shape as the in-tree AgentInfo struct (service.go:13). We don't
// trust the individual step's own check — the whole point of
// outside-in assertions is to re-verify via the same CLI a
// production operator would use.
type cliAgent struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"`
}

// TestAgentsSmoke is the seventh Multipass integration test: it
// runs provisioning + security + gateway + channels + agents on a
// single-machine manifest-agents.yml fixture (one channel, one
// agent) and asserts, via a mix of the openclaw CLI and a direct
// SFTP pull of openclaw.json / the workspace files, that every
// sub-step of the agents phase actually landed the expected state.
//
// What this exercises that none of the earlier tests do:
//
//   - agents.AddAgentStep: `openclaw agents add` — writes the
//     agents.list[] entry with id/workspace/model. First test
//     calling into openclaw's agents CLI at all.
//   - agents.EnsureModelStep: `openclaw config set --batch-json`
//     targeting agents.list[0].model (+ .fallbacks when set). No
//     fallback in this fixture; that's an "Applicable but no drift"
//     case worth asserting to prove Check's short-circuit on
//     matching model survives the apply.
//   - agents.ConfigureWorkspaceStep: writes SOUL.md, AGENTS.md,
//     IDENTITY.md into the agent's workspace directory inside CLAWS
//     MANAGED START/END markers, then `openclaw agents set-identity
//     --name --emoji`. This is the first test that SFTPs files out
//     of the VM to assert on their bytes (channels SFTPs openclaw.
//     json but that's a config file, not a workspace artifact).
//   - agents.ConfigureToolsStep: `tools.elevated.enabled` +
//     `tools.elevated.allowFrom.telegram` via `config set`. No
//     tools.exec here — the agent config has no Exec block, so
//     buildExecBatch short-circuits and only the elevated keys get
//     written. A second test with the node phase will cover
//     tools.exec later.
//   - agents.ConfigureBindingsStep: `openclaw agents bind --agent
//     main --bind telegram:telegram-main`. Proves the
//     AgentBinding.Channel/.Account round-trip from manifest →
//     formatBinding → CLI survives.
//
// Why loopback bind + single channel + single agent:
//
//   This test is deliberately the SMALLEST complete exercise of the
//   agents phase. Every other phase has its own dedicated smoke
//   test; we DON'T want this one to fail for reasons unrelated to
//   agents. A nil networking block means no mesh infrastructure to
//   fight with; one channel means no ensure-default-account to
//   recheck (TestChannelsSmoke already owns that); one agent avoids
//   the maxAgentConcurrency=1 bottleneck AND the
//   "concurrent agent-adds race on openclaw.json writes" bug
//   (docs/issues/03-agent-phase-concurrency.md) that would be a
//   red herring for first-time agents coverage.
//
// Runtime budget: same shape as TestChannelsSmoke (~4 min) plus
// ~30s for the agents phase (one `openclaw agents add` + three
// `config set` calls + `agents bind`, each paying the TypeScript
// startup tax on first invocation, amortized after node-compile-
// cache warms). 25min cap absorbs cold apt/npm caches.
func TestAgentsSmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-agents.yml")
	m.Prefix = "it-ag-" + randSuffix(t)
	prefix := m.Prefix
	// --- fake token plumbing ------------------------------------------------
	//
	// Parallel-safe env-var injection via env_file. See
	// injectFakeTelegramTokens for the rationale (t.Setenv
	// is incompatible with t.Parallel and would race on
	// TELEGRAM_MAIN_BOT_TOKEN across sibling tests).
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
	if ag.Workspace == "" {
		t.Fatalf("fixture sanity: agent %q has empty workspace", ag.ID)
	}
	if ag.Model.Primary == "" {
		t.Fatalf("fixture sanity: agent %q has empty model.primary", ag.ID)
	}
	// Tools.Elevated being set (not nil) is what makes
	// ConfigureToolsStep.Applicable return true. If a future
	// refactor drops the elevated block from the fixture the step
	// silently skips and this test no longer covers it — fail loud.
	if ag.Tools == nil || ag.Tools.Elevated == nil {
		t.Fatalf("fixture sanity: agent %q must declare tools.elevated to exercise ConfigureToolsStep; got tools=%+v",
			ag.ID, ag.Tools)
	}
	if ag.Tools.Exec != nil {
		t.Fatalf("fixture sanity: agent %q must NOT declare tools.exec (needs a node, covered by a follow-up test); got %+v",
			ag.ID, ag.Tools.Exec)
	}
	if ag.Identity == nil || ag.Identity.Name == "" {
		t.Fatalf("fixture sanity: agent %q must declare identity.name to exercise agents set-identity", ag.ID)
	}
	if ag.Soul == "" || ag.AgentsMD == "" {
		t.Fatalf("fixture sanity: agent %q must declare soul + agents_md to exercise configure-workspace", ag.ID)
	}
	if len(ag.Bindings) != 1 {
		t.Fatalf("fixture sanity: expected exactly 1 binding, got %d", len(ag.Bindings))
	}
	if ag.Bindings[0].Channel != "telegram" || ag.Bindings[0].Account != "telegram-main" {
		t.Fatalf("fixture sanity: binding[0] = {channel:%q, account:%q}, want {telegram, telegram-main}",
			ag.Bindings[0].Channel, ag.Bindings[0].Account)
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

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		// Debug hook: CLAWS_IT_KEEP_VMS skips the cleanup sweep
		// so a failing test leaves the VMs running for manual
		// SSH inspection. Do not set in CI.
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

	t.Log("applying provisioning + security + gateway + channels + agents on 1 VM")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		// Mesh phase is auto-added (noop because nil networking)
		// but we skip it explicitly — predictable phase frontier.
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

	// ---- gateway-still-healthy cross-check -----------------------------------
	//
	// Run these BEFORE the agents-specific assertions. Any
	// regression where the agents phase destabilizes the gateway
	// daemon (e.g. a config-set race that corrupts openclaw.json
	// mid-write and the daemon reloads a truncated file) shows up
	// here as "systemd inactive" / "port not listening" — a clear
	// pointer to the agents→gateway interaction — rather than as
	// a noisy downstream agents assertion failure.

	assertGatewayUnitActive(t, dial, host, mc)
	assertGatewayPortListening(t, dial, host, mc)

	// ---- agents-phase outside-in assertions ----------------------------------

	// 1. `openclaw agents list --json` reports the agent by id,
	//    with the manifest model. This is the single canonical
	//    "did add-agent + ensure-model both land?" signal, and
	//    matches the surface production operators use to verify
	//    agent registration.
	assertAgentRegistered(t, dial, host, mc, ag.ID, ag.Model.Primary)

	// 2. `openclaw config get agents.list[0].id` via typed JSON pull
	//    (and sibling scalars). A regression that writes to a
	//    legacy path (e.g. agents.registered[] instead of agents.
	//    list[]) would leave the struct's List empty and produce
	//    a clean failure; the CLI-based check in (1) would also
	//    break, but the struct-level failure points at the shape
	//    mismatch specifically.
	cfg := fetchAgentsPhaseConfig(t, dial, host, mc)
	assertAgentInConfig(t, mc.Name, cfg, ag.ID, ag.Model.Primary, ag.Workspace)

	// 3. tools.elevated.enabled + allowFrom landed. Reading via
	//    struct decode (not just `config get tools.elevated.enabled`)
	//    proves the allowFrom nested map also round-tripped — a
	//    regression that writes the scalar but drops the map would
	//    pass a plain string-valued config get.
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

	// 4. channels.telegram.accounts.telegram-main got the fake
	//    token (unredacted, via SFTP). Proves the channels phase
	//    still ran to completion even with the agents phase
	//    layered on top — the bindings step depends on this
	//    account existing, so a missing token here would also
	//    poison `agents bind`.
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

	// 5. Workspace files exist on disk with the managed markers
	//    and the manifest content between them. SFTPs each file
	//    and asserts on the bytes directly — the daemon doesn't
	//    consume these files, but any future feature (e.g. a soul-
	//    drift detector) would, so "bytes on disk match manifest"
	//    is the correct contract.
	assertWorkspaceManagedFile(t, dial, host, mc, ag.Workspace, "SOUL.md", ag.Soul)
	assertWorkspaceManagedFile(t, dial, host, mc, ag.Workspace, "AGENTS.md", string(ag.AgentsMD))
	// IDENTITY.md is built by buildIdentityMD (configure_workspace.
	// go:63). Asserting a substring, not the whole body, so a
	// stylistic header change in buildIdentityMD doesn't force a
	// test rewrite; the critical invariant is that name + emoji
	// actually made it into the file.
	assertWorkspaceFileContains(t, dial, host, mc, ag.Workspace, "IDENTITY.md",
		"name: "+ag.Identity.Name)
	assertWorkspaceFileContains(t, dial, host, mc, ag.Workspace, "IDENTITY.md",
		"emoji: "+ag.Identity.Emoji)

	// 6. `openclaw agents bindings --agent main --json` returns
	//    the (channel=telegram, account=telegram-main) tuple.
	//    Mirrors the step's own Verify surface (configure_bindings.
	//    go:109) so a schema drift on the bindings JSON would
	//    break the step AND this test together — exactly the
	//    right tightly-coupled test. Note: `agents bindings`
	//    is a separate command from `agents list`, and the
	//    on-disk storage path for bindings isn't documented here
	//    — we deliberately DON'T try to parse it out of
	//    openclaw.json, because we'd rather trust the stable CLI
	//    than guess at the internal schema.
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
}

// ---------------------------------------------------------------------------
// agents-phase assertion helpers
// ---------------------------------------------------------------------------

// assertAgentRegistered verifies `openclaw agents list --json`
// contains the expected id + model. Mirrors the in-tree ListAgents
// helper (service.go:23) — same JSON shape, same tolerance for a
// CLI banner before the JSON payload (cold node-compile-cache
// starts print a "Loading config from ..." preamble on the first
// invocation after boot).
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

// assertAgentBinding verifies `openclaw agents bindings --agent <id>
// --json` returns the expected (channel, account) tuple.
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

// shellQuote wraps s in single quotes for use inside a `bash -c`
// invocation. The agent IDs this test uses are simple identifiers
// (no shell metachars), but quoting defensively keeps this helper
// reusable if a future test uses richer IDs.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fetchAgentsPhaseConfig SFTPs ~/.openclaw/openclaw.json off the
// gateway VM and decodes the narrow subset we care about. Mirrors
// the Channels test's fetchOpenclawConfig (which returns a
// different narrow struct for a different phase) — deliberately
// separate rather than shared because the "what subset matters to
// this test" is test-local.
//
// Hardcoded home path (/home/<agent>) is correct for every fixture
// in this package (all ubuntu24.04). If we ever add an image where
// HOME differs, resolve via `echo $HOME` over SSH instead.
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

// assertAgentInConfig scans agents.list for an entry with the
// expected id and verifies its scalar fields. Split from the CLI
// check so a "list command happy but config file broken" scenario
// (which would indicate the daemon serves a cached in-memory view
// of a stale on-disk file) produces two distinct failures instead
// of one.
//
// Workspace check uses strings.Contains on the suffix — the
// openclaw CLI may or may not resolve `~/…` when it writes the
// entry, so we tolerate either form. The STRONG check is that
// it's non-empty and contains the trailing path component; the
// resolution semantics are the CLI's business.
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
			// Accept either "~/.openclaw/workspace" (literal)
			// or "/home/<user>/.openclaw/workspace" (resolved)
			// — whichever the CLI chose to persist.
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

// resolveWorkspaceDir returns the absolute path of an agent's
// workspace directory on the remote VM. Mirrors the in-tree
// resolveWorkspace (configure_workspace.go:76) — but runs in the
// test harness instead of over SSH, using the hardcoded
// /home/<agent> mapping we already rely on for the config file.
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

// assertWorkspaceManagedFile SFTPs the remote workspace file and
// asserts its bytes contain the CLAWS MANAGED markers AND the
// manifest content between them. Uses substring matching (not
// byte-for-byte equality) because configure-workspace wraps
// content with a trailing separator + "Everything below is yours"
// line on first write — that's part of the managed file's public
// shape, but the assertion here focuses on the managed-section
// invariant, not the user-section boilerplate.
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
	// Match on the trimmed content so trailing-whitespace or
	// YAML-to-file normalization doesn't false-positive a
	// mismatch. writeManagedSection itself also calls
	// strings.TrimSpace before writing, so both sides are
	// comparable.
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

// assertWorkspaceFileContains is a looser variant of
// assertWorkspaceManagedFile for files whose body is synthesized
// (buildIdentityMD wraps name: / emoji: lines inside its own
// header). We don't want to hardcode the full synthesized body
// because a cosmetic change in the builder would needlessly break
// the test; instead we assert on a single meaningful substring.
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

// firstLine returns the first line of s — used in error messages
// to keep them short without dropping the useful "what were we
// looking for" context.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
