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

// Fake bot tokens — see the Multipass tier's linode_channels_test.go
// (sibling file in ../multipass) for the full rationale. Short
// version: telegram's setup adapter only checks for token presence,
// not validity, so any non-empty string lets `openclaw channels add`
// write the config. The daemon's getUpdates poll later 401s against
// api.telegram.org and backs off; gateway health is unaffected.
//
// The values are deliberately different from the Multipass tier's
// constants so a grep for either leaked token points at the exact
// test file that planted it, even if logs get cross-contaminated in
// CI. Format (10-digit bot ID : payload) matches Telegram's shape
// so a future string-level sanity pass in openclaw can't reject
// them on syntactic grounds.
const (
	fakeTelegramMainToken      = "000000001:FAKE_CLAWS_IT_LINODE_MAIN_TOKEN_DO_NOT_USE_XXXXX"
	fakeTelegramSecondaryToken = "000000001:FAKE_CLAWS_IT_LINODE_SECONDARY_DO_NOT_USE_XXXXX"
)

// openclawConfig is the minimal slice of ~/.openclaw/openclaw.json
// we care about for this test. Kept in sync shape-wise with the
// Multipass tier's struct — if openclaw's config schema evolves
// in a way that affects channel accounts, both tiers need updating
// together anyway so the duplication is intentional (and narrow
// enough to review at a glance).
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

// TestChannelsSmoke is the Linode counterpart to the Multipass tier's
// TestChannelsSmoke. It runs provisioning + security + gateway +
// channels on a single-machine, two-telegram-channel manifest and
// asserts the channels phase landed the config (both accounts
// present, default set correctly, unredacted tokens on disk) without
// destabilizing the already-running gateway daemon.
//
// Why this Linode mirror is worth the dollar-pennies cost over the
// Multipass tier's identical assertions:
//
//   - `openclaw channels add` resolves the telegram extension
//     through the public npm registry on first invocation. A
//     regression in how the CLI finds bundled extensions (e.g. a
//     path-resolution bug that only bites when /usr/lib/node_modules
//     is the global prefix, which Linode's ubuntu24.04 uses but
//     Multipass's slimmer image may not) would fail here first.
//   - The loopback-bind invariant under channels phase activity on
//     a public-IP VM. Provisioning + security + gateway on Linode
//     already prove the gateway binds 127.0.0.1:18789. This test
//     re-asserts the same after the channels phase runs, so a
//     future regression where `openclaw channels add` inadvertently
//     flips gateway.bind to "lan" (it shouldn't — they're separate
//     config subtrees — but the reload-after-mutation code path is
//     exercised for the first time here) would be caught BEFORE
//     reaching a production gateway on a real public IP.
//   - The daemon's config-file watcher + reload cycle under
//     channels-phase-induced writes on real cloud kernel. Multipass
//     shares the host kernel; Linode doesn't. Inotify edge cases
//     between hypervisor filesystems and the Tailwind-y Node.js
//     watcher occasionally show up here first.
//
// Fake tokens are safe on Linode for the exact same reason they're
// safe on Multipass: apply-time asserts happen between SSH dial and
// config file write, Telegram API is never hit during the phases
// under test. The gateway daemon later 401s in the background.
//
// Cost envelope: ~10 min on one g6-standard-1 at us-east ≈ $0.003.
// 20min cap absorbs cold apt mirrors and npm registry latency.
func TestChannelsSmoke(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-channels.yml")
	m.Prefix = "it-lin-ch-" + randSuffix(t)
	prefix := m.Prefix
	// --- fake token plumbing ------------------------------------------------
	//
	// Parallel-safe env-var injection via env_file. t.Setenv is
	// incompatible with t.Parallel and would race sibling tests
	// on the same TELEGRAM_*_BOT_TOKEN name; the helper rewrites
	// each TokenEnv to a per-test-unique name and drops the fakes
	// into a tempdir env_file. See injectFakeTelegramTokens.
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
	if len(m.Nodes) != 0 {
		t.Fatalf("fixture sanity: expected 0 nodes, got %d", len(m.Nodes))
	}

	gw := &m.Gateways[0]
	if gw.Name != "gateway" {
		t.Fatalf("fixture sanity: expected gateway name \"gateway\", got %q", gw.Name)
	}
	// Two-channel requirement: see Multipass tier's equivalent check.
	// With one channel the ensure-default-account assertion below
	// becomes a tautology (first-seen == only-seen == default), and
	// a regression in desiredDefaults() that ignores ch.Default
	// would slip through silently.
	if len(gw.Channels) != 2 {
		t.Fatalf("fixture sanity: expected 2 channels, got %d", len(gw.Channels))
	}
	if !gw.Channels[0].Default {
		t.Fatalf("fixture sanity: gw.Channels[0] (%q) must have default: true",
			gw.Channels[0].Name)
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
	// Loopback-only invariant. Same rationale as TestGatewaySmoke —
	// on a public-IP Linode instance a sneaking-in networking block
	// would promote this to a real-mesh test, we'd lose the
	// loopback regression guard, AND the test would need headscale
	// + caddy infrastructure we aren't providing.
	if gw.Networking != nil {
		t.Fatalf("fixture sanity: gateway %q must not have a networking block (loopback bind only), got %+v",
			gw.Name, gw.Networking)
	}

	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
		}
		// Same guard as TestGatewaySmoke: if agent_user flips to
		// root, the systemd --user + linger coverage collapses
		// because root doesn't need linger.
		if mc.AgentUser == "root" {
			t.Fatalf("fixture sanity: machine %q agent_user is 'root' — "+
				"TestChannelsSmoke needs a dedicated agent account to "+
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

	for _, want := range []string{"provisioning", "security", "gateway", "channels"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("applying provisioning + security + gateway + channels on 1 Linode instance")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
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
	// Run these BEFORE the channels-specific assertions so a
	// regression where channels phase somehow destabilizes the
	// daemon shows up as "[gateway] systemd inactive" / "port not
	// listening" — a clear pointer to the channels→gateway
	// interaction — rather than as a noisy downstream channels
	// assertion failure.
	//
	// The loopback-bind re-check on Linode is the single most
	// valuable assertion in this block: a public-IP VM running
	// openclaw-gateway with channels configured must keep binding
	// 127.0.0.1:18789. A future regression that flips the bind
	// during a channels-phase reload would be caught here before
	// reaching production.

	assertGatewayUnitActive(t, dial, host, mc)
	assertGatewayPortListening(t, dial, host, mc)

	// ---- channels-phase outside-in assertions -----------------------------------

	// 1. `openclaw channels list --json` reports both accounts —
	//    add-channels landed twice. Mirrors the production Go
	//    step's own check path (channels/add_channel.go:40).
	assertChannelAccountRegistered(t, dial, host, mc, "telegram", "telegram-main")
	assertChannelAccountRegistered(t, dial, host, mc, "telegram", "telegram-secondary")

	// 2. `openclaw config get channels.telegram.defaultAccount`
	//    returns "telegram-main" — the highest-value single
	//    assertion in the whole test (see Multipass tier's
	//    equivalent comment for the full rationale).
	assertOpenclawConfigValue(t, dial, host, mc,
		"channels.telegram.defaultAccount", "telegram-main")

	// 3. The raw openclaw.json on disk has both unredacted tokens
	//    at the multi-bot schema path. We SFTP the file down and
	//    parse it locally in Go — the CLI's `config get` redacts
	//    secrets, but the daemon reads the file unredacted, and
	//    that's the view we need to verify. A regression that
	//    writes to the legacy single-bot path
	//    (channels.telegram.botToken) would leave Accounts empty
	//    and both assertAccountToken calls would fail loudly.
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
	if after := waitForEmptyListByTag(ctx, t, prov, "claws/"+prefix, 30*time.Second); len(after) != 0 {
		t.Errorf("ListByTag after destroy still returned %d instances: %+v", len(after), after)
	}
}

// ---------------------------------------------------------------------------
// channels-phase assertion helpers (Linode-local)
//
// These mirror the Multipass tier's helpers rather than extracting
// them to a shared package. Shared integration-test helpers are a
// maintenance trap — the build tags differ, the package-level
// helpers (loadToken, dialer, manifest loader) differ, and once
// we'd started sharing we'd be pulled into building yet another
// "common" package that's effectively a subset of testing. Cheap
// to duplicate now; re-evaluate if we add a third provider tier.
// ---------------------------------------------------------------------------

// assertChannelAccountRegistered parses `openclaw channels list
// --json` and verifies the (kind, name) tuple is present under
// `chat.<kind>`. Mirror of the in-tree ListChannelAccounts helper
// (internal/claws/plans/apply/channels/service.go:21) — same shape,
// same preamble tolerance, so a schema drift would be caught by the
// in-tree step AND this test in the same commit.
func assertChannelAccountRegistered(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, kind, name string) {
	t.Helper()
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, `openclaw channels list --json 2>/dev/null`)
	if err != nil {
		t.Errorf("[%s] openclaw channels list --json: %v\n%s", mc.Name, err, out)
		return
	}
	// CLI sometimes prints a banner before the JSON body on cold
	// node-compile-cache starts — seek to the first '{' instead of
	// assuming the whole output is pure JSON.
	idx := strings.IndexByte(out, '{')
	if idx < 0 {
		t.Errorf("[%s] channels list output had no JSON body:\n%s", mc.Name, out)
		return
	}
	raw := out[idx:]
	var payload struct {
		Chat map[string][]string `json:"chat"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Errorf("[%s] parse channels list: %v\nraw:\n%s", mc.Name, err, raw)
		return
	}
	accounts := payload.Chat[kind]
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
// downloads ~/.openclaw/openclaw.json, and decodes it into an
// openclawConfig. Bypasses the openclaw CLI's secret redaction so
// we can assert real token values — same mechanism as the Multipass
// tier's equivalent helper.
//
// Home path hardcoded to /home/<agent> — correct for Linode's stock
// ubuntu24.04 image. If a future fixture uses a base image where
// HOME differs, resolve it via an SSH `echo $HOME` instead of
// hardcoding.
func fetchOpenclawConfig(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) openclawConfig {
	t.Helper()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	configPath := path.Join("/home", user, ".openclaw", "openclaw.json")

	// 20s is generous for <10KB JSON file over SFTP on a us-east
	// link. If this starts timing out, investigate sshd — don't
	// bump the budget.
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
		snippet := string(raw)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "… (truncated)"
		}
		t.Fatalf("[%s] decode openclaw.json: %v\nraw:\n%s", mc.Name, err, snippet)
	}
	t.Logf("[%s] fetched openclaw.json (%d bytes) via sftp", mc.Name, len(raw))
	return cfg
}

// assertAccountToken: same split-failure-mode pattern as the
// Multipass helper ("account present?" is distinct from "token
// matches?" so a missing-account regression produces a cleaner
// failure than a generic empty-vs-want botToken error).
func assertAccountToken(t *testing.T, machineName string, cfg openclawConfig, name, want string) {
	t.Helper()
	acc, ok := cfg.Channels.Telegram.Accounts[name]
	if !ok {
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
