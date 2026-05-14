//go:build integration_multipass

package multipass

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
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// openclawConfig is the minimal subset of ~/.openclaw/openclaw.json
// this test cares about. Keeping it narrow means a schema migration
// that adds or reorders unrelated keys won't break the test — only
// changes to the channels.telegram.* subtree matter. If openclaw
// ever moves channels under e.g. `gateway.channels.telegram`, this
// struct's zero-value deserialize will flag it immediately (both
// Accounts and DefaultAccount will be empty, asserted below).
type openclawConfig struct {
	Channels struct {
		Telegram struct {
			DefaultAccount string                             `json:"defaultAccount"`
			Accounts       map[string]openclawTelegramAccount `json:"accounts"`
		} `json:"telegram"`
	} `json:"channels"`
}

type openclawTelegramAccount struct {
	BotToken string `json:"botToken"`
}

// Fake bot tokens consumed by `openclaw channels add`. Telegram's
// setup adapter (extensions/telegram/src/setup-core.ts, the
// `telegramSetupAdapter` const) validates tokens with `Boolean(input.
// token || input.tokenFile)` — any non-empty string passes. The
// daemon will later fail getUpdates polling against api.telegram.org
// with 401 and back off exponentially, which is why we also assert
// the gateway health port stays up after channels apply.
//
// Format matches Telegram's <botID>:<base64ish> shape so the CLI's
// string-level sanity pass can't reject it on a future-hardened
// adapter. The "000000000" bot ID is specifically NOT a real one
// (bot IDs start in the 10⁸ range and are sequentially issued).
const (
	fakeTelegramMainToken      = "000000000:FAKE_CLAWS_IT_MAIN_TOKEN_DO_NOT_USE_XXXXXXXXXXXXXXX"
	fakeTelegramSecondaryToken = "000000000:FAKE_CLAWS_IT_SECOND_TOKEN_DO_NOT_USE_XXXXXXXXXXXX"
)

// TestChannelsSmoke is the fifth Multipass integration test: it runs
// provisioning + security + gateway + channels on a single-machine,
// two-channel manifest-channels.yml fixture and asserts, over SSH,
// that the channels phase actually landed the account config AND
// set the correct default account.
//
// This test is deliberately SEPARATE from TestGatewaySmoke so a
// regression in add-channels / ensure-default-account surfaces here
// with an unambiguous "it's a channels bug" signal, not folded into
// a larger test that might already be failing for an unrelated
// reason. If TestGatewaySmoke goes red the gateway test fails; if
// the channels phase goes red this one fails; you never have to
// bisect which phase broke.
//
// What this exercises that TestGatewaySmoke doesn't:
//
//   - channels.BuildChannelTargets' token resolver — reads
//     TELEGRAM_{MAIN,SECONDARY}_BOT_TOKEN from the process env via
//     manifestsvc.LookupEnvFromManifest. t.Setenv below plants the
//     values; no env_file needed on the manifest.
//   - channels.AddChannelStep (`add-channels`): shells
//     `openclaw channels add --channel telegram --account telegram-
//     main --token 000000000:FAKE...` for each channel. Writes
//     channels.telegram.accounts.<name>.botToken in
//     ~/.openclaw/openclaw.json. Idempotency is tested by the Check
//     method during plan execution.
//   - channels.EnsureDefaultStep (`ensure-default-account`): shells
//     `openclaw config set channels.telegram.defaultAccount
//     telegram-main` because `default: true` is on the first
//     channel. The SINGLE most valuable assertion in this test is
//     that this landed correctly — with only one channel, a broken
//     desiredDefaults() would silently fall back to "first seen"
//     and the bug would be invisible.
//   - The interaction between the already-running gateway daemon and
//     an `openclaw config set` running in the CLI alongside it. The
//     daemon watches the config file for changes and hot-reloads;
//     the RunWithConflictAndTransientRetry wrapper exists precisely
//     because the two processes can race on file writes
//     (ConfigMutationConflictError). We don't force concurrency
//     here, but the retry path is dormant code until a real apply
//     triggers it — this test is the first to exercise it.
//
// Why fake tokens are safe for this test surface:
//
//   Everything we assert happens between the SSH dial and the config
//   file write on the VM. The Telegram API never gets hit during
//   apply. When the gateway daemon later tries getUpdates against
//   api.telegram.org with these tokens, it sees 401 Unauthorized on
//   the first request, backs off (minutes between retries), and the
//   gateway service-stays-up invariant holds. No secrets leak, no
//   real bot gets abused, no rate-limit pressure on Telegram's edge.
//
// Runtime budget: same shape as TestGatewaySmoke (~3.5 min) plus
// ~15–30s for the channels phase (two `openclaw channels add`
// invocations + one `config set`, each a TypeScript-heavy CLI
// invocation). 20min cap absorbs cold caches.
func TestChannelsSmoke(t *testing.T) {
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

	m := loadTestManifest(t, "manifest-channels.yml")
	m.Prefix = "it-ch-" + randSuffix(t)
	prefix := m.Prefix
	// --- fake token plumbing ------------------------------------------------
	//
	// Route the fake channel tokens through an env_file with
	// per-test-unique var names (see injectFakeTelegramTokens).
	// t.Setenv isn't usable here — it refuses to run under
	// t.Parallel and would clobber sibling tests' token values
	// anyway because it mutates the single process environment.
	// The env_file lives in t.TempDir so the fake tokens never
	// touch the git-indexed testdata tree.
	injectFakeTelegramTokens(t, m, map[string]string{
		"telegram-main":      fakeTelegramMainToken,
		"telegram-secondary": fakeTelegramSecondaryToken,
	})

	if len(m.Machines) != 1 {
		t.Fatalf("fixture sanity: expected 1 machine, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	gw := &m.Gateways[0]
	if gw.Name != "gateway" {
		t.Fatalf("fixture sanity: expected gateway name \"gateway\", got %q", gw.Name)
	}
	// The whole point of a two-channel fixture is to prove
	// ensure-default-account actually honors ch.Default instead of
	// falling back to "first seen". If the fixture ever drifts to
	// one channel, that assertion becomes a tautology.
	if len(gw.Channels) != 2 {
		t.Fatalf("fixture sanity: expected 2 channels (to distinguish default from first-seen), got %d",
			len(gw.Channels))
	}
	// Explicit default on the first channel matches production
	// (david-army.yml). Flipping the order would still work, but
	// make it asymmetric on purpose so reordering channels during
	// refactoring would flip the expected default and fail loudly.
	if !gw.Channels[0].Default {
		t.Fatalf("fixture sanity: gw.Channels[0] (%q) must have default: true", gw.Channels[0].Name)
	}
	if gw.Channels[1].Default {
		t.Fatalf("fixture sanity: gw.Channels[1] (%q) must NOT have default: true",
			gw.Channels[1].Name)
	}
	for _, ch := range gw.Channels {
		if ch.Kind != manifestdata.ChannelKindTelegram {
			t.Fatalf("fixture sanity: channel %q kind = %q, want %q",
				ch.Name, ch.Kind, manifestdata.ChannelKindTelegram)
		}
		if ch.TokenEnv == "" {
			t.Fatalf("fixture sanity: channel %q has empty token_env", ch.Name)
		}
	}
	if gw.Networking != nil {
		t.Fatalf("fixture sanity: gateway must not have a networking block (loopback bind), got %+v",
			gw.Networking)
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

	for _, want := range []string{"provisioning", "security", "gateway", "channels"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + gateway + channels on 1 VM")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		// Mesh phase is auto-added (noop because no networking
		// block) but there's no reason to execute it. Explicit
		// allowlist keeps the phase frontier small and the runtime
		// envelope predictable.
		OnlyPhases: []string{"provisioning", "security", "gateway", "channels"},
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
	// Run the "gateway is still up" assertions FIRST. A regression
	// where the channels phase somehow crashes the daemon or leaves
	// systemd in a bad state would show up here, not in the
	// channels-specific assertions below. Keeping them first means a
	// red "[gateway] systemd inactive" failure is a clear signal to
	// look at channel-apply side effects, not at a broken
	// ensure-default-account script.

	assertGatewayUnitActive(t, dial, host, mc)
	assertGatewayPortListening(t, dial, host, mc)

	// ---- channels-phase outside-in assertions -----------------------------------

	// 1. `openclaw channels list --json` reports both telegram
	//    accounts. This is the canonical "did add-channels land?"
	//    signal; matches the Check/Verify implementations in
	//    add_channel.go that the step itself uses.
	assertChannelAccountRegistered(t, dial, host, mc, "telegram", "telegram-main")
	assertChannelAccountRegistered(t, dial, host, mc, "telegram", "telegram-secondary")

	// 2. `openclaw config get channels.telegram.defaultAccount`
	//    returns "telegram-main" — the one we marked default: true.
	//    This is the SINGLE highest-value assertion in the test:
	//    with two accounts and an explicit default flag, a broken
	//    desiredDefaults() that ignores ch.Default would set
	//    "telegram-main" anyway (first seen == explicitly default),
	//    so the check is weaker than it looks UNLESS we also
	//    eventually cover the "default on second channel" case.
	//    That's a future follow-up; this test covers the common
	//    production case (default: true on first listed channel,
	//    matching david-army.yml).
	assertOpenclawConfigValue(t, dial, host, mc,
		"channels.telegram.defaultAccount", "telegram-main")

	// 3. The raw openclaw.json on disk has exactly the shape the
	//    daemon reads. We SFTP the file, parse it locally with
	//    Go's json decoder, and assert on the typed struct — much
	//    stronger than the CLI-based checks above, because:
	//      - the CLI redacts secrets (__OPENCLAW_REDACTED__), so
	//        we can't verify token values via `config get`
	//      - a regression that writes the token to a legacy path
	//        like channels.telegram.botToken (single-bot shape)
	//        would still make `channels list` + `config get
	//        defaultAccount` look green, but the parsed struct
	//        below would show empty Accounts and catch it
	//      - arbitrary future fields become trivial to assert by
	//        just extending the openclawConfig struct
	cfg := fetchOpenclawConfig(t, dial, host, mc)
	if got := cfg.Channels.Telegram.DefaultAccount; got != "telegram-main" {
		t.Errorf("[%s] openclaw.json channels.telegram.defaultAccount = %q, want %q",
			mc.Name, got, "telegram-main")
	}
	assertAccountToken(t, mc.Name, cfg, "telegram-main", fakeTelegramMainToken)
	assertAccountToken(t, mc.Name, cfg, "telegram-secondary", fakeTelegramSecondaryToken)

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
// channels-phase assertion helpers
//
// These use the package-shared sshRunAsGatewayAgent helper (defined
// in multipass_gateway_test.go). Dial-as-agent is correct: the whole
// gateway + channels config tree lives under ~agent, not ~root.
// ---------------------------------------------------------------------------

// channelEntry is the new JSON structure for each channel in `openclaw channels list --json`.
// The format changed from map[string][]string to map[string]{ accounts: []string, ... }.
type channelEntry struct {
	Accounts  []string `json:"accounts"`
	Installed bool     `json:"installed"`
	Origin    string   `json:"origin"`
}

// assertChannelAccountRegistered parses `openclaw channels list --json`
// and confirms the (kind, name) tuple is present under `chat.<kind>`.
// Matches the shape ListChannelAccounts expects in service.go (kind
// -> []accountName). If the JSON shape drifts, this helper will
// fail with a clear parse error rather than silently "not finding"
// the account.
func assertChannelAccountRegistered(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, kind, name string) {
	t.Helper()
	// Match the Go step's own listing command so the test is
	// literally exercising the same surface the phase uses at apply
	// time. Using `openclaw channels list` (not `--json`) here
	// would pass with a broken JSON shape, which defeats the point.
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, `openclaw channels list --json 2>/dev/null`)
	if err != nil {
		t.Errorf("[%s] openclaw channels list --json: %v\n%s", mc.Name, err, out)
		return
	}
	// The openclaw CLI occasionally prints a banner before the
	// JSON payload (e.g. "Loading config from ..." on first
	// invocation of a cold node-compile-cache process). The in-
	// tree ListChannelAccounts helper (service.go:26) handles
	// this by seeking to the first '{'; mirror that so the test
	// isn't flaky on cold starts.
	idx := strings.IndexByte(out, '{')
	if idx < 0 {
		t.Errorf("[%s] channels list output had no JSON body:\n%s", mc.Name, out)
		return
	}
	raw := out[idx:]

	// Try new format first: { "chat": { "telegram": { "accounts": [...], ... } } }
	var newPayload struct {
		Chat map[string]channelEntry `json:"chat"`
	}
	var accounts []string
	if err := json.Unmarshal([]byte(raw), &newPayload); err == nil && newPayload.Chat != nil {
		if entry, ok := newPayload.Chat[kind]; ok {
			accounts = entry.Accounts
		}
	} else {
		// Legacy format fallback: { "chat": { "telegram": ["account1", ...] } }
		var legacyPayload struct {
			Chat map[string][]string `json:"chat"`
		}
		if err := json.Unmarshal([]byte(raw), &legacyPayload); err != nil {
			t.Errorf("[%s] parse channels list: %v\nraw:\n%s", mc.Name, err, raw)
			return
		}
		accounts = legacyPayload.Chat[kind]
	}

	for _, got := range accounts {
		if got == name {
			t.Logf("[%s] channels list: %s/%s registered", mc.Name, kind, name)
			return
		}
	}
	t.Errorf("[%s] account %s/%s not found in channels list; got %s=%v (full raw: %s)",
		mc.Name, kind, name, kind, accounts, out)
}

// fetchOpenclawConfig opens an SFTP session as the agent user,
// downloads ~/.openclaw/openclaw.json into memory, and decodes it
// into an openclawConfig struct. The whole point of pulling the
// file (vs. shelling `cat` over SSH) is to bypass the openclaw
// CLI's secret redaction: `config get ...botToken` returns the
// __OPENCLAW_REDACTED__ sentinel, but the on-disk JSON has the
// real value. This gives tests the same unredacted view the daemon
// sees at startup.
//
// On test failure (missing file, permission error, malformed JSON)
// this calls t.Fatalf — there's nothing useful an individual
// assertion can do with a half-parsed config, and continuing would
// just produce noisy "default of empty struct is empty" errors.
//
// Home path is hardcoded as /home/<agent>. That's correct for the
// Ubuntu 24.04 cloud image every fixture in this package uses;
// if we ever add an image where the agent's HOME isn't /home/<user>,
// resolving the path via an SSH `echo $HOME` would be the fix.
func fetchOpenclawConfig(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) openclawConfig {
	t.Helper()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	configPath := path.Join("/home", user, ".openclaw", "openclaw.json")

	// 20s is generous for a single sftp fetch of a <10KB JSON
	// file, but matches the rest of the gateway-phase helpers'
	// tolerance for sshd handshake jitter under load. If this
	// ever starts timing out, investigate sshd — don't just bump
	// the budget.
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

	var cfg openclawConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// Truncate the dumped JSON on failure — a 5KB config file
		// dumped into the test log buries everything else and
		// makes CI output unreadable. 512 bytes is enough to
		// eyeball where the decoder choked.
		snippet := string(raw)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "… (truncated)"
		}
		t.Fatalf("[%s] decode openclaw.json: %v\nraw:\n%s", mc.Name, err, snippet)
	}
	t.Logf("[%s] fetched openclaw.json (%d bytes) via sftp", mc.Name, len(raw))
	return cfg
}

// assertAccountToken verifies that cfg has an account under
// channels.telegram.accounts.<name> whose botToken equals want. We
// split the "is the account present at all?" check from the "does
// its token match?" check so a regression that forgets to create
// the account entirely produces a clearer failure than a generic
// "empty botToken != fakeTelegramMainToken".
func assertAccountToken(t *testing.T, machineName string, cfg openclawConfig, name, want string) {
	t.Helper()
	acc, ok := cfg.Channels.Telegram.Accounts[name]
	if !ok {
		// List the keys we did see so a drift in account naming
		// (e.g. a normalization pass that lowercased dashes)
		// shows up directly rather than as "key missing".
		keys := make([]string, 0, len(cfg.Channels.Telegram.Accounts))
		for k := range cfg.Channels.Telegram.Accounts {
			keys = append(keys, k)
		}
		t.Errorf("[%s] channels.telegram.accounts[%q] missing; got keys=%v",
			machineName, name, keys)
		return
	}
	if acc.BotToken != want {
		t.Errorf("[%s] channels.telegram.accounts[%q].botToken = %q, want %q",
			machineName, name, acc.BotToken, want)
	}
}
