package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// Relative paths (resolved against the SFTP session's default cwd =
// the SSH user's home) of the daemon-owned files the snapshot reader
// reads. openclaw.json is the main user-facing config; pending.json and
// paired.json are the device-pairing state files the daemon writes
// synchronously as pairings move through states.
const (
	remoteOpenclawConfigPath = ".openclaw/openclaw.json"
	remotePendingDevicesPath = ".openclaw/nodes/pending.json"
	remotePairedDevicesPath  = ".openclaw/devices/paired.json"
)

// NewSnapshotConfigReader returns a ConfigReader that, when the plan is
// in its probe phase (scaffold.IsProbeActive), answers reads from a
// single per-host SFTP snapshot of ~/.openclaw/openclaw.json. Outside
// the probe phase every call delegates to the fallback CLI reader so
// Execute observes writes made by preceding steps.
//
// The snapshot is cached in the plan cache keyed by {addr, port, user},
// so probing N agents × M read-ops ends up as exactly 1 SFTP read per
// gateway per plan run.
//
// fallback is what handles "no snapshot available" cases (e.g. pre-probe
// or post-probe calls, or a fresh machine where the config file doesn't
// exist yet but the CLI still answers with defaults).
func NewSnapshotConfigReader(fallback ConfigReader) ConfigReader {
	return &snapshotConfigReader{fallback: fallback}
}

// DefaultConfigReader is the standard reader wired into every phase's
// Options: a snapshot reader layered over a CLI reader. Check paths see
// the snapshot's speedup during probe; Execute paths see the CLI's
// freshness guarantee.
func DefaultConfigReader(dial SSHDialFunc) ConfigReader {
	return NewSnapshotConfigReader(NewCLIConfigReader(dial))
}

type snapshotConfigReader struct {
	fallback ConfigReader

	// loadMu serialises first-time loads per host within a single
	// process so concurrent probes for the same gateway don't race
	// multiple SFTP reads of the same file. Shared between the
	// openclaw.json and devices snapshots — both reads are cheap and
	// holding one mutex for both keeps the lock discipline simple.
	loadMu sync.Mutex
}

func snapshotCacheKey(h ConfigHost) string {
	return fmt.Sprintf("OPENCLAW_SNAPSHOT|%s|%d|%s", h.Addr, h.Port, h.User)
}

func devicesSnapshotCacheKey(h ConfigHost) string {
	return fmt.Sprintf("OPENCLAW_DEVICES_SNAPSHOT|%s|%d|%s", h.Addr, h.Port, h.User)
}

// loadSnapshot returns (or creates and caches) the openclaw.json snapshot
// for h during the probe phase. Returns nil when the probe phase is not
// active — that's how callers know to fall through to the CLI reader.
//
// When the remote config file does not exist yet (fresh machine), we
// cache an empty snapshot so every read answers "no agents, no bindings,
// no channels", which matches what the CLI would report.
func (r *snapshotConfigReader) loadSnapshot(ctx context.Context, client *xssh.Client, h ConfigHost) (*openclawSnapshot, error) {
	if !scaffold.IsProbeActive(ctx) {
		return nil, nil
	}
	key := snapshotCacheKey(h)
	if v, ok := scaffold.PlanCacheGet(ctx, key); ok {
		if snap, ok := v.(*openclawSnapshot); ok {
			return snap, nil
		}
	}

	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	// Double-check under the lock: another goroutine may have populated it.
	if v, ok := scaffold.PlanCacheGet(ctx, key); ok {
		if snap, ok := v.(*openclawSnapshot); ok {
			return snap, nil
		}
	}

	snap, err := fetchSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	scaffold.PlanCacheSet(ctx, key, snap)
	return snap, nil
}

// fetchSnapshot does the actual SFTP read + parse. Returns an empty
// snapshot (no error) when the file doesn't exist so probes against
// fresh machines don't fail.
func fetchSnapshot(ctx context.Context, client *xssh.Client, h ConfigHost) (*openclawSnapshot, error) {
	// sshfile.ReadFile does not take a context; we check before the call
	// so probe cancellation is honoured promptly.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("snapshot reader: nil SSH client for %s@%s:%d", h.User, h.Addr, h.Port)
	}
	data, err := sshfile.ReadFile(client, remoteOpenclawConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return &openclawSnapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot: read %s on %s: %w", remoteOpenclawConfigPath, h.Addr, err)
	}
	snap := &openclawSnapshot{}
	if err := json.Unmarshal(data, snap); err != nil {
		return nil, fmt.Errorf("snapshot: parse %s on %s: %w", remoteOpenclawConfigPath, h.Addr, err)
	}
	return snap, nil
}

// openclawSnapshot is the subset of ~/.openclaw/openclaw.json the probe
// Check functions need. Anything not listed here is intentionally ignored —
// a schema bump that adds or reorders unrelated keys must not break this
// reader.
type openclawSnapshot struct {
	Agents struct {
		List     []snapshotAgent   `json:"list"`
		Defaults snapshotAgentDefs `json:"defaults"`
	} `json:"agents"`
	Bindings []snapshotBinding                `json:"bindings"`
	Channels map[string]snapshotChannelConfig `json:"channels"`
	Tools    struct {
		Elevated *snapshotElevated `json:"elevated"`
	} `json:"tools"`
	Gateway snapshotGateway `json:"gateway"`
}

// snapshotGateway mirrors the cfg.gateway subset that configure-gateway
// compares against the manifest. Unset keys decode to the zero value,
// which matches how ReadConfigValue returns "" for a missing key.
type snapshotGateway struct {
	Mode string `json:"mode"`
	Bind string `json:"bind"`
}

type snapshotAgentDefs struct {
	// Model accepts either a bare string or {primary, fallbacks} — see
	// openclaw's config schema. Capture it raw and normalise at read time.
	Model json.RawMessage `json:"model"`
}

type snapshotAgent struct {
	ID    string          `json:"id"`
	Model json.RawMessage `json:"model"`
	Tools *snapshotTools  `json:"tools"`
}

type snapshotTools struct {
	Exec *RemoteExecConfig `json:"exec"`
}

type snapshotElevated struct {
	Enabled   *bool               `json:"enabled"`
	AllowFrom map[string][]string `json:"allowFrom"`
}

type snapshotBinding struct {
	Type    string `json:"type"`
	AgentID string `json:"agentId"`
	Match   struct {
		Channel   string `json:"channel"`
		AccountID string `json:"accountId"`
	} `json:"match"`
}

type snapshotChannelConfig struct {
	DefaultAccount string                     `json:"defaultAccount"`
	Accounts       map[string]json.RawMessage `json:"accounts"`
}

// extractModelPrimary decodes openclaw's two-shape model field into the
// primary string. Accepts:
//
//   - bare string:   "anthropic-claude-4"          → "anthropic-claude-4"
//   - object form:   {"primary":"x","fallbacks":[…]} → "x"
//   - unset/null:    ""                            → ""
//
// Mirrors the toAgentModel() logic inside openclaw's agents.config.ts so
// our snapshot reader returns the same primary the `openclaw agents list
// --json` CLI would emit.
func extractModelPrimary(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var asObject struct {
		Primary string `json:"primary"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		return strings.TrimSpace(asObject.Primary)
	}
	return ""
}

// extractModelFallbacks decodes openclaw's two-shape model field into the
// fallbacks slice. Accepts:
//
//   - bare string:   "anthropic-claude-4"            → nil
//   - object form:   {"primary":"x","fallbacks":[…]} → fallbacks slice
//   - unset/null:    ""                              → nil
//
// Returns nil (not empty slice) when there are no fallbacks.
func extractModelFallbacks(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	// Bare string form has no fallbacks
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return nil
	}
	// Object form may have fallbacks
	var asObject struct {
		Fallbacks []string `json:"fallbacks"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		return asObject.Fallbacks
	}
	return nil
}

// normalizeAgentID matches openclaw's normaliseAgentId: trim + case-fold.
// Kept private because callers shouldn't need to know; the snapshot
// reader applies it on both sides of every ID comparison.
func normalizeAgentID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// ---------------------------------------------------------------------------
// ConfigReader impl
// ---------------------------------------------------------------------------

func (r *snapshotConfigReader) AgentConfigIndex(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (int, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return -1, err
	}
	if snap == nil {
		return r.fallback.AgentConfigIndex(ctx, client, h, agentID)
	}
	target := normalizeAgentID(agentID)
	for i, a := range snap.Agents.List {
		if normalizeAgentID(a.ID) == target {
			return i, nil
		}
	}
	return -1, nil
}

func (r *snapshotConfigReader) AgentModel(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (string, bool, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return "", false, err
	}
	if snap == nil {
		return r.fallback.AgentModel(ctx, client, h, agentID)
	}
	target := normalizeAgentID(agentID)
	for _, a := range snap.Agents.List {
		if normalizeAgentID(a.ID) != target {
			continue
		}
		m := extractModelPrimary(a.Model)
		if m == "" {
			m = extractModelPrimary(snap.Agents.Defaults.Model)
		}
		return m, true, nil
	}
	return "", false, nil
}

func (r *snapshotConfigReader) AgentModelFull(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (string, []string, bool, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return "", nil, false, err
	}
	if snap == nil {
		return r.fallback.AgentModelFull(ctx, client, h, agentID)
	}
	target := normalizeAgentID(agentID)
	for _, a := range snap.Agents.List {
		if normalizeAgentID(a.ID) != target {
			continue
		}
		m := extractModelPrimary(a.Model)
		f := extractModelFallbacks(a.Model)
		if m == "" {
			m = extractModelPrimary(snap.Agents.Defaults.Model)
			// Note: we don't inherit fallbacks from defaults, matching openclaw CLI behavior
		}
		return m, f, true, nil
	}
	return "", nil, false, nil
}

func (r *snapshotConfigReader) AgentTools(ctx context.Context, client *xssh.Client, h ConfigHost, idx int) (*RemoteToolsConfig, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return r.fallback.AgentTools(ctx, client, h, idx)
	}
	out := &RemoteToolsConfig{}
	if idx < 0 || idx >= len(snap.Agents.List) {
		return out, nil
	}
	if a := snap.Agents.List[idx]; a.Tools != nil {
		out.Exec = a.Tools.Exec
	}
	return out, nil
}

func (r *snapshotConfigReader) Elevated(ctx context.Context, client *xssh.Client, h ConfigHost) (*RemoteElevatedConfig, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return r.fallback.Elevated(ctx, client, h)
	}
	out := &RemoteElevatedConfig{}
	if snap.Tools.Elevated != nil {
		out.Enabled = snap.Tools.Elevated.Enabled
		out.AllowFrom = snap.Tools.Elevated.AllowFrom
	}
	return out, nil
}

func (r *snapshotConfigReader) AgentBindings(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) ([]RemoteBinding, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return r.fallback.AgentBindings(ctx, client, h, agentID)
	}
	target := normalizeAgentID(agentID)
	var out []RemoteBinding
	for _, b := range snap.Bindings {
		if b.Type != "route" {
			continue
		}
		if normalizeAgentID(b.AgentID) != target {
			continue
		}
		out = append(out, RemoteBinding{
			AgentID: b.AgentID,
			Channel: b.Match.Channel,
			Account: b.Match.AccountID,
		})
	}
	return out, nil
}

func (r *snapshotConfigReader) ChannelAccounts(ctx context.Context, client *xssh.Client, h ConfigHost) (map[string][]string, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return r.fallback.ChannelAccounts(ctx, client, h)
	}
	out := map[string][]string{}
	for kind, cfg := range snap.Channels {
		if len(cfg.Accounts) == 0 {
			continue
		}
		ids := make([]string, 0, len(cfg.Accounts))
		for id := range cfg.Accounts {
			ids = append(ids, id)
		}
		out[kind] = ids
	}
	return out, nil
}

func (r *snapshotConfigReader) DefaultAccount(ctx context.Context, client *xssh.Client, h ConfigHost, kind string) (string, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return "", err
	}
	if snap == nil {
		return r.fallback.DefaultAccount(ctx, client, h, kind)
	}
	if cfg, ok := snap.Channels[kind]; ok {
		return strings.TrimSpace(cfg.DefaultAccount), nil
	}
	return "", nil
}

func (r *snapshotConfigReader) GatewayView(ctx context.Context, client *xssh.Client, h ConfigHost) (GatewayView, error) {
	snap, err := r.loadSnapshot(ctx, client, h)
	if err != nil {
		return GatewayView{}, err
	}
	if snap == nil {
		return r.fallback.GatewayView(ctx, client, h)
	}
	return GatewayView{
		Mode: strings.TrimSpace(snap.Gateway.Mode),
		Bind: strings.TrimSpace(snap.Gateway.Bind),
	}, nil
}

// DeviceList serves from a devices snapshot (pending.json + paired.json)
// during probe; delegates to the CLI fallback otherwise. The two files
// are daemon state, not user config, so they live under a separate
// cache key and are read with a separate SFTP round-trip — but still
// just once per host per plan.
func (r *snapshotConfigReader) DeviceList(ctx context.Context, client *xssh.Client, h ConfigHost) (*DeviceList, error) {
	dl, err := r.loadDevicesSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	if dl == nil {
		return r.fallback.DeviceList(ctx, client, h)
	}
	return dl, nil
}

// loadDevicesSnapshot returns (nil, nil) when the probe phase is not
// active (callers fall through to the CLI reader), otherwise returns
// the per-host cached inventory, populating the cache on first call.
//
// A missing file (fresh install) decodes to an empty DeviceList; the
// daemon writes both files synchronously on state transitions so an
// empty file is indistinguishable from "no entries" here.
func (r *snapshotConfigReader) loadDevicesSnapshot(ctx context.Context, client *xssh.Client, h ConfigHost) (*DeviceList, error) {
	if !scaffold.IsProbeActive(ctx) {
		return nil, nil
	}
	key := devicesSnapshotCacheKey(h)
	if v, ok := scaffold.PlanCacheGet(ctx, key); ok {
		if dl, ok := v.(*DeviceList); ok {
			return dl, nil
		}
	}

	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	if v, ok := scaffold.PlanCacheGet(ctx, key); ok {
		if dl, ok := v.(*DeviceList); ok {
			return dl, nil
		}
	}

	dl, err := fetchDevicesSnapshot(ctx, client, h)
	if err != nil {
		return nil, err
	}
	scaffold.PlanCacheSet(ctx, key, dl)
	return dl, nil
}

// fetchDevicesSnapshot reads both on-disk state files over SFTP and
// converges them on the common DeviceList shape. Parse failures on
// either file collapse to "no entries for that half" rather than a
// hard error, matching the CLI reader's behaviour — the upshot is the
// snapshot never fails a probe just because the daemon happened to be
// mid-write when we read.
func fetchDevicesSnapshot(ctx context.Context, client *xssh.Client, h ConfigHost) (*DeviceList, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("snapshot reader: nil SSH client for %s@%s:%d", h.User, h.Addr, h.Port)
	}
	dl := &DeviceList{}

	if raw, err := sshfile.ReadFile(client, remotePendingDevicesPath); err == nil {
		var m map[string]struct {
			RequestID   string `json:"requestId"`
			DisplayName string `json:"displayName"`
		}
		if err := json.Unmarshal(raw, &m); err == nil {
			for k, v := range m {
				rid := v.RequestID
				if rid == "" {
					rid = k
				}
				dl.Pending = append(dl.Pending, PendingDevice{
					RequestID:   rid,
					DisplayName: v.DisplayName,
					Role:        "node",
				})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("snapshot: read %s on %s: %w", remotePendingDevicesPath, h.Addr, err)
	}

	if raw, err := sshfile.ReadFile(client, remotePairedDevicesPath); err == nil {
		var m map[string]struct {
			DisplayName string   `json:"displayName"`
			ClientID    string   `json:"clientId"`
			ClientMode  string   `json:"clientMode"`
			Scopes      []string `json:"scopes"`
		}
		if err := json.Unmarshal(raw, &m); err == nil {
			for deviceID, v := range m {
				dl.Paired = append(dl.Paired, PairedDevice{
					DeviceID:    deviceID,
					DisplayName: v.DisplayName,
					ClientID:    v.ClientID,
					ClientMode:  v.ClientMode,
					Scopes:      v.Scopes,
				})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("snapshot: read %s on %s: %w", remotePairedDevicesPath, h.Addr, err)
	}

	return dl, nil
}
