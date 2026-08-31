package common

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	xssh "golang.org/x/crypto/ssh"
)

// NewCLIConfigReader returns a ConfigReader backed by `openclaw …` CLI
// invocations over SSH. Every method shells out to the remote openclaw
// binary; correctness-wise this is the ground truth (it's exactly what
// happened before this layer existed) but every call pays a Node-cold-start
// + daemon-IPC tax measurable in the tens of seconds per invocation on a
// fresh machine. Prefer [NewSnapshotConfigReader], which wraps this one
// and short-circuits probe-time reads to a single SFTP snapshot.
func NewCLIConfigReader(dial SSHDialFunc) ConfigReader {
	return &cliConfigReader{dial: dial}
}

type cliConfigReader struct {
	dial SSHDialFunc
}

// withClient borrows (or reuses) a client and invokes fn with it.
func (r *cliConfigReader) withClient(ctx context.Context, existing *xssh.Client, h ConfigHost, fn func(*xssh.Client) error) error {
	if existing != nil {
		return fn(existing)
	}
	client, key, err := BorrowSSH(ctx, r.dial, h.Addr, h.Port, h.User)
	if err != nil {
		return err
	}
	defer ReturnSSH(ctx, key, client)
	return fn(client)
}

func (r *cliConfigReader) runJSON(client *xssh.Client, script string, startChar byte) (string, error) {
	out, err := bash.RunOutput(client, OpenclawCLIPreamble()+script)
	if err != nil {
		return "", err
	}
	return extractJSON(strings.TrimSpace(out), startChar), nil
}

// cliAgent is one normalized roster entry, independent of the on-disk
// shape (legacy agents.list array vs. 2026.8.1+ agents.entries map).
type cliAgent struct {
	ID     string                    `json:"id"`
	Model  json.RawMessage           `json:"model"`
	Models map[string]map[string]any `json:"models"`
	Tools  *struct {
		Exec *RemoteExecConfig `json:"exec"`
	} `json:"tools"`
}

// fetchAgentRoster reads the agent roster and normalizes both on-disk
// shapes to an ordered slice.
//
// OpenClaw 2026.8.1 reshaped agents.list (array) into agents.entries (map
// keyed by agent id). Crucially the read side is asymmetric: on 2026.8.1
// `config get agents.list` returns a hard error ("Unknown config path"),
// while writes via the legacy indexed path still work through a shim. The
// old `config get agents.list --json 2>/dev/null || echo "[]"` idiom turned
// that loud breaking change into a silent empty roster — which made every
// agent read-back come up empty, breaking add-agent verify and destroying
// idempotency for the whole agents phase.
//
// Strategy: probe agents.entries first (the canonical 2026.8.1 path); if
// that path is unknown (older CLI), fall back to agents.list. A genuine
// error on the chosen path propagates instead of being swallowed.
//
// The map shape carries the id in the key, so we fold it back into each
// entry's ID and sort by id for a stable order (index positions are only
// ever used to build a path for the same agent they were resolved for).
func (r *cliConfigReader) fetchAgentRoster(c *xssh.Client) ([]cliAgent, error) {
	// Try the 2026.8.1+ map shape first.
	entriesRaw, entriesErr := r.runJSON(c, `openclaw config get agents.entries --json`, '{')
	if entriesErr == nil {
		var entries map[string]cliAgent
		if err := json.Unmarshal([]byte(entriesRaw), &entries); err == nil {
			ids := make([]string, 0, len(entries))
			for id := range entries {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			out := make([]cliAgent, 0, len(ids))
			for _, id := range ids {
				a := entries[id]
				if strings.TrimSpace(a.ID) == "" {
					a.ID = id
				}
				out = append(out, a)
			}
			return out, nil
		}
		// entries path answered but didn't parse as a map — fall through to
		// the legacy shape rather than guessing.
	}

	// Legacy (≤ 2026.7.x) array shape.
	listRaw, listErr := r.runJSON(c, `openclaw config get agents.list --json`, '[')
	if listErr != nil {
		// Both shapes failed. Surface the errors instead of silently
		// reporting an empty roster (which is what created the original bug).
		return nil, fmt.Errorf("read agent roster: entries: %v; list: %w", entriesErr, listErr)
	}
	var list []cliAgent
	if err := json.Unmarshal([]byte(listRaw), &list); err != nil {
		return nil, fmt.Errorf("parse agents.list %q: %w", listRaw, err)
	}
	return list, nil
}

func (r *cliConfigReader) AgentConfigIndex(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (int, error) {
	idx := -1
	target := normalizeAgentID(agentID)
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		agents, err := r.fetchAgentRoster(c)
		if err != nil {
			return fmt.Errorf("agent config index: %w", err)
		}
		for i, entry := range agents {
			if normalizeAgentID(entry.ID) == target {
				idx = i
				return nil
			}
		}
		return nil
	})
	return idx, err
}

func (r *cliConfigReader) AgentModel(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (string, bool, error) {
	var model string
	var exists bool
	target := normalizeAgentID(agentID)
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		agents, err := r.fetchAgentRoster(c)
		if err != nil {
			return fmt.Errorf("agent model: %w", err)
		}
		for _, a := range agents {
			if normalizeAgentID(a.ID) != target {
				continue
			}
			exists = true
			model = extractModelPrimary(a.Model)
			return nil
		}
		return nil
	})
	return model, exists, err
}

func (r *cliConfigReader) AgentModelFull(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (string, []string, bool, error) {
	var model string
	var fallbacks []string
	var exists bool
	target := normalizeAgentID(agentID)
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		agents, err := r.fetchAgentRoster(c)
		if err != nil {
			return fmt.Errorf("agent model: %w", err)
		}
		for _, a := range agents {
			if normalizeAgentID(a.ID) != target {
				continue
			}
			exists = true
			model = extractModelPrimary(a.Model)
			fallbacks = extractModelFallbacks(a.Model)
			return nil
		}
		return nil
	})
	return model, fallbacks, exists, err
}

func (r *cliConfigReader) AgentModelEntries(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (map[string]map[string]any, bool, error) {
	entries := map[string]map[string]any{}
	var exists bool
	target := normalizeAgentID(agentID)
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		agents, err := r.fetchAgentRoster(c)
		if err != nil {
			return fmt.Errorf("agent model entries: %w", err)
		}
		for _, a := range agents {
			if normalizeAgentID(a.ID) != target {
				continue
			}
			exists = true
			for ref, entry := range a.Models {
				entries[ref] = entry
			}
			return nil
		}
		return nil
	})
	if !exists {
		return nil, false, err
	}
	return entries, true, err
}

// AgentTools resolves tools.exec for the agent at roster index idx. It
// reads the roster (entries-first) and indexes into the normalized slice
// rather than building an `agents.list[%d].tools` path directly — that
// indexed config path is a legacy shim on 2026.8.1 and, for reads,
// `agents.list` is entirely unknown there. The index is produced by
// AgentConfigIndex against the same normalized roster, so positions line up.
func (r *cliConfigReader) AgentTools(ctx context.Context, client *xssh.Client, h ConfigHost, idx int) (*RemoteToolsConfig, error) {
	cfg := &RemoteToolsConfig{}
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		agents, err := r.fetchAgentRoster(c)
		if err != nil {
			return fmt.Errorf("agent tools: %w", err)
		}
		if idx < 0 || idx >= len(agents) {
			return nil // out of range → unset, matches prior behavior
		}
		if t := agents[idx].Tools; t != nil {
			cfg.Exec = t.Exec
		}
		return nil
	})
	return cfg, err
}

func (r *cliConfigReader) Elevated(ctx context.Context, client *xssh.Client, h ConfigHost) (*RemoteElevatedConfig, error) {
	cfg := &RemoteElevatedConfig{}
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		out, err := bash.RunOutput(c, OpenclawCLIPreamble()+
			`openclaw config get tools.elevated --json 2>/dev/null || echo "null"`)
		if err != nil {
			return err
		}
		raw := strings.TrimSpace(out)
		if raw == "null" || raw == "" {
			return nil
		}
		raw = extractJSON(raw, '{')
		_ = json.Unmarshal([]byte(raw), cfg) // parse-failure → empty, matches prior
		return nil
	})
	return cfg, err
}

func (r *cliConfigReader) AgentBindings(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) ([]RemoteBinding, error) {
	var out []RemoteBinding
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		raw, err := r.runJSON(c, fmt.Sprintf(
			`openclaw agents bindings --agent %q --json 2>/dev/null || echo "[]"`, agentID), '[')
		if err != nil {
			return fmt.Errorf("agent bindings: %w", err)
		}
		var bindings []struct {
			AgentID string `json:"agentId"`
			Match   struct {
				Channel   string `json:"channel"`
				AccountID string `json:"accountId"`
			} `json:"match"`
		}
		if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
			return nil // matches prior behavior: parse failure → empty
		}
		out = make([]RemoteBinding, 0, len(bindings))
		for _, b := range bindings {
			out = append(out, RemoteBinding{
				AgentID: b.AgentID,
				Channel: b.Match.Channel,
				Account: b.Match.AccountID,
			})
		}
		return nil
	})
	return out, err
}

// ChannelAccounts reads cfg.channels.<kind>.accounts directly via
// `openclaw config get channels --json`, not `openclaw channels list
// --json`. The two disagree: `channels list` synthesizes/reports entries
// beyond what's actually persisted (observed live — an account referenced
// only by a route binding showed up in `channels list` while genuinely
// absent from `config get` and from the config file itself). This step's
// only use of ChannelAccounts is "does this account already exist so I can
// skip adding it" — trusting the inflated view made add-channels silently
// no-op on a real missing account. `config get` is the same source the
// snapshot reader uses (an SFTP read of the config file), so both readers
// now agree by construction.
func (r *cliConfigReader) ChannelAccounts(ctx context.Context, client *xssh.Client, h ConfigHost) (map[string][]string, error) {
	out := map[string][]string{}
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		raw, err := r.runJSON(c, `openclaw config get channels --json 2>/dev/null || echo "{}"`, '{')
		if err != nil {
			return nil // matches prior behavior: soft-fail to empty
		}

		var payload map[string]struct {
			Accounts map[string]json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return nil
		}
		for kind, entry := range payload {
			ids := make([]string, 0, len(entry.Accounts))
			for id := range entry.Accounts {
				ids = append(ids, id)
			}
			out[kind] = ids
		}
		return nil
	})
	return out, err
}

func (r *cliConfigReader) DefaultAccount(ctx context.Context, client *xssh.Client, h ConfigHost, kind string) (string, error) {
	var v string
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		key := fmt.Sprintf("channels.%s.defaultAccount", kind)
		out, err := bash.RunOutput(c, OpenclawCLIPreamble()+fmt.Sprintf(
			`openclaw config get %s 2>/dev/null || echo ""`, key))
		if err != nil {
			return nil // matches prior behavior
		}
		v = strings.TrimSpace(out)
		return nil
	})
	return v, err
}

func (r *cliConfigReader) GatewayView(ctx context.Context, client *xssh.Client, h ConfigHost) (GatewayView, error) {
	var v GatewayView
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		mode, err := readConfigScalar(c, "gateway.mode")
		if err != nil {
			return fmt.Errorf("read gateway.mode: %w", err)
		}
		bind, err := readConfigScalar(c, "gateway.bind")
		if err != nil {
			return fmt.Errorf("read gateway.bind: %w", err)
		}
		v = GatewayView{Mode: mode, Bind: bind}
		return nil
	})
	return v, err
}

// readConfigScalar runs `openclaw config get <key>` and returns the
// trimmed value. Matches the legacy gateway.ReadConfigValue helper: we
// sentinel-missing with "__missing__" so a genuinely empty value survives
// the round-trip while an unset key collapses to "".
func readConfigScalar(client *xssh.Client, key string) (string, error) {
	out, err := bash.RunOutput(client, OpenclawCLIPreamble()+fmt.Sprintf(
		`openclaw config get %s 2>/dev/null || echo "__missing__"`, key))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(out)
	if v == "__missing__" {
		return "", nil
	}
	return v, nil
}

// DeviceList prefers `openclaw devices list --json` and falls back to
// reading the daemon's pending/paired JSON files directly when the CLI
// times out. The fallback is load-bearing on 1-vCPU hosts where Node.js
// cold-start blocks past the CLI's 10 s gateway-handshake deadline; the
// daemon writes those files synchronously so they're an authoritative,
// CLI-free view of the same information.
func (r *cliConfigReader) DeviceList(ctx context.Context, client *xssh.Client, h ConfigHost) (*DeviceList, error) {
	var out *DeviceList
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		if dl, ok := listDevicesViaCLI(c); ok {
			out = dl
			return nil
		}
		out = listDevicesFromDisk(c)
		return nil
	})
	return out, err
}

func listDevicesViaCLI(client *xssh.Client) (*DeviceList, bool) {
	// Use gateway token to bypass CLI device auth, which may have a pending
	// scope upgrade that blocks the connection.
	token := ReadGatewayAuthToken(client)

	var script string
	if token != "" {
		// --url ensures loopback detection so the CLI omits device identity
		// and uses pure token auth. Without --url, the CLI may send device
		// identity on the first call and create an unwanted pairing.
		script = OpenclawCLIPreamble() + fmt.Sprintf(`openclaw devices list --json --token %q --url ws://127.0.0.1:18789 2>/dev/null`, token)
	} else {
		script = OpenclawCLIPreamble() + `openclaw devices list --json 2>/dev/null`
	}

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return nil, false
	}
	raw := strings.TrimSpace(out)
	if raw == "" {
		return nil, false
	}
	var dl DeviceList
	if err := json.Unmarshal([]byte(raw), &dl); err != nil {
		return nil, false
	}
	return &dl, true
}

// listDevicesFromDisk reads the daemon's pending + paired state files
// directly via `cat` over SSH. Shape of the files (observed on openclaw
// 2026.4.x):
//
//	~/.openclaw/nodes/pending.json
//	  { "<requestId>": { "requestId": ..., "displayName": ..., ... } }
//
//	~/.openclaw/devices/paired.json
//	  { "<deviceId>":  { "deviceId": ..., "displayName": ...,
//	                     "clientMode": "node"|"cli", "role": ..., ... } }
//
// Missing / empty files collapse to an empty list — fresh install vs.
// transient read miss looks the same to callers, who already retry.
func listDevicesFromDisk(client *xssh.Client) *DeviceList {
	dl := &DeviceList{}

	if raw, ok := readDaemonStateFile(client, `~/.openclaw/nodes/pending.json`); ok {
		var m map[string]struct {
			RequestID   string `json:"requestId"`
			DisplayName string `json:"displayName"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
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
	}

	if raw, ok := readDaemonStateFile(client, `~/.openclaw/devices/paired.json`); ok {
		var m map[string]struct {
			DisplayName string `json:"displayName"`
			ClientMode  string `json:"clientMode"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			for _, v := range m {
				dl.Paired = append(dl.Paired, PairedDevice{
					DisplayName: v.DisplayName,
					ClientMode:  v.ClientMode,
				})
			}
		}
	}

	return dl
}

func readDaemonStateFile(client *xssh.Client, path string) (string, bool) {
	out, err := bash.RunOutput(client, fmt.Sprintf(`cat %s 2>/dev/null || true`, path))
	if err != nil {
		return "", false
	}
	raw := strings.TrimSpace(out)
	if raw == "" || raw == "{}" {
		return "", false
	}
	return raw, true
}

// extractJSON finds the first occurrence of startChar ('{' or '[') in s and
// returns from there onward. Openclaw's CLI sometimes prints daemon-status
// preamble before the JSON payload; callers strip it so the unmarshal sees
// a clean document.
// ReadGatewayAuthToken reads the gateway auth token from the remote
// machine's config file via SFTP. Returns "" when the token cannot
// be read. Uses SFTP instead of `openclaw config get` because the
// CLI redacts secret values.
func ReadGatewayAuthToken(client *xssh.Client) string {
	home, err := bash.RunOutput(client, `echo $HOME`)
	if err != nil {
		return ""
	}
	// Import is in the gateway package; call the SFTP-based reader
	// that already handles gateway.auth.token correctly.
	raw, err := sshfile.ReadFile(client, strings.TrimSpace(home)+"/.openclaw/openclaw.json")
	if err != nil {
		return ""
	}
	var cfg struct {
		Gateway struct {
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return cfg.Gateway.Auth.Token
}

func extractJSON(s string, startChar byte) string {
	idx := strings.IndexByte(s, startChar)
	if idx < 0 {
		if startChar == '[' {
			return "[]"
		}
		return "{}"
	}
	return s[idx:]
}
