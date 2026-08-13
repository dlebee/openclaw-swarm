package common

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
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

func (r *cliConfigReader) AgentConfigIndex(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (int, error) {
	idx := -1
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		raw, err := r.runJSON(c, `openclaw config get agents.list --json 2>/dev/null || echo "[]"`, '[')
		if err != nil {
			return fmt.Errorf("agent config index: %w", err)
		}
		var list []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil // treat parse failure as "not found" — matches prior behavior
		}
		for i, entry := range list {
			if entry.ID == agentID {
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
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		raw, err := r.runJSON(c, `openclaw agents list --json 2>/dev/null || echo "[]"`, '[')
		if err != nil {
			return fmt.Errorf("agents list: %w", err)
		}
		var agents []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(raw), &agents); err != nil {
			return fmt.Errorf("agents list: parse %q: %w", raw, err)
		}
		for _, a := range agents {
			if a.ID == agentID {
				model = a.Model
				exists = true
				return nil
			}
		}
		return nil
	})
	return model, exists, err
}

func (r *cliConfigReader) AgentModelFull(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (string, []string, bool, error) {
	var model string
	var fallbacks []string
	var exists bool
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		// Read from config directly to get the full model object including fallbacks
		raw, err := r.runJSON(c, `openclaw config get agents.list --json 2>/dev/null || echo "[]"`, '[')
		if err != nil {
			return fmt.Errorf("agents list: %w", err)
		}
		var agents []struct {
			ID    string          `json:"id"`
			Model json.RawMessage `json:"model"`
		}
		if err := json.Unmarshal([]byte(raw), &agents); err != nil {
			return fmt.Errorf("agents list: parse %q: %w", raw, err)
		}
		for _, a := range agents {
			if a.ID == agentID {
				exists = true
				// Try object form first
				var modelObj struct {
					Primary   string   `json:"primary"`
					Fallbacks []string `json:"fallbacks"`
				}
				if err := json.Unmarshal(a.Model, &modelObj); err == nil && modelObj.Primary != "" {
					model = modelObj.Primary
					fallbacks = modelObj.Fallbacks
					return nil
				}
				// Try string form
				var modelStr string
				if err := json.Unmarshal(a.Model, &modelStr); err == nil {
					model = modelStr
					fallbacks = nil
					return nil
				}
				return nil
			}
		}
		return nil
	})
	return model, fallbacks, exists, err
}

func (r *cliConfigReader) AgentTools(ctx context.Context, client *xssh.Client, h ConfigHost, idx int) (*RemoteToolsConfig, error) {
	cfg := &RemoteToolsConfig{}
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		key := fmt.Sprintf("agents.list[%d].tools", idx)
		raw, err := r.runJSON(c, fmt.Sprintf(`openclaw config get %s --json 2>/dev/null || echo "{}"`, key), '{')
		if err != nil {
			return err
		}
		var parsed struct {
			Exec *RemoteExecConfig `json:"exec"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil // treat parse failure as unset — matches prior behavior
		}
		cfg.Exec = parsed.Exec
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

// channelEntry is the new JSON structure for each channel in `openclaw channels list --json`.
// The format changed from map[string][]string to map[string]{ accounts: []string, ... }.
type channelEntry struct {
	Accounts  []string `json:"accounts"`
	Installed bool     `json:"installed"`
	Origin    string   `json:"origin"`
}

func (r *cliConfigReader) ChannelAccounts(ctx context.Context, client *xssh.Client, h ConfigHost) (map[string][]string, error) {
	out := map[string][]string{}
	err := r.withClient(ctx, client, h, func(c *xssh.Client) error {
		raw, err := r.runJSON(c, `openclaw channels list --json 2>/dev/null || echo "{}"`, '{')
		if err != nil {
			return nil // matches prior behavior: soft-fail to empty
		}

		// New format: { "chat": { "telegram": { "accounts": [...], "installed": true, "origin": "configured" } } }
		var newPayload struct {
			Chat map[string]channelEntry `json:"chat"`
		}
		if err := json.Unmarshal([]byte(raw), &newPayload); err == nil && newPayload.Chat != nil {
			for kind, entry := range newPayload.Chat {
				out[kind] = entry.Accounts
			}
			return nil
		}

		// Legacy format fallback: { "chat": { "telegram": ["account1", "account2"] } }
		var legacyPayload struct {
			Chat map[string][]string `json:"chat"`
		}
		if err := json.Unmarshal([]byte(raw), &legacyPayload); err != nil {
			return nil
		}
		if legacyPayload.Chat != nil {
			out = legacyPayload.Chat
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
	token := readGatewayAuthToken(client)

	var script string
	if token != "" {
		script = OpenclawCLIPreamble() + fmt.Sprintf(`openclaw devices list --json --token %q 2>/dev/null`, token)
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
// machine's openclaw config. Returns "" when the token cannot be read.
// It tries the canonical path (gateway.auth.token) first, then falls
// back to the deprecated top-level gateway.token for older installs.
func ReadGatewayAuthToken(client *xssh.Client) string {
	for _, key := range []string{"gateway.auth.token", "gateway.token"} {
		script := OpenclawCLIPreamble() + fmt.Sprintf(`openclaw config get %s 2>/dev/null`, key)
		out, err := bash.RunOutput(client, script)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(out)
		if v != "" && !strings.HasPrefix(v, "Config") && v != "__missing__" {
			return v
		}
	}
	return ""
}

// readGatewayAuthToken is the package-internal alias.
func readGatewayAuthToken(client *xssh.Client) string {
	return ReadGatewayAuthToken(client)
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
