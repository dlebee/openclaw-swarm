package common

import (
	"context"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	xssh "golang.org/x/crypto/ssh"
)

// ConfigReader exposes read-only queries against an openclaw gateway's
// runtime config (~/.openclaw/openclaw.json). Step Check/Verify functions
// call into a ConfigReader instead of shelling out to `openclaw …` CLIs
// directly, which lets the plan swap in a snapshot-backed implementation
// during the probe phase for ~30× faster reads.
//
// Implementations MUST be safe for concurrent use. Every method accepts an
// optional already-borrowed *xssh.Client — when non-nil, implementations
// may reuse it to avoid a redundant dial; when nil they borrow/dial their
// own. Host is always required as the cache key for per-host snapshots.
//
// Invariants:
//
//   - During the probe phase ([scaffold.IsProbeActive] == true) a reader
//     MAY cache values across calls for the same Host.
//   - Outside the probe phase ([scaffold.IsProbeActive] == false) every
//     call MUST observe the live config, so Execute sees writes made by
//     preceding steps in the same run.
type ConfigReader interface {
	// AgentConfigIndex returns the 0-based index of agentID within
	// cfg.agents.list[]. Returns (-1, nil) if the agent is absent.
	AgentConfigIndex(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (int, error)

	// AgentModel returns the effective primary model for agentID (as
	// reported by `openclaw agents list --json`'s `.model` field),
	// and whether the agent is present in agents.list[]. Agents whose
	// model is unset inherit cfg.agents.defaults.model.
	AgentModel(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (model string, exists bool, err error)

	// AgentModelFull returns the full model configuration (primary + fallbacks)
	// for agentID. Returns exists=false if the agent is not in agents.list[].
	// Fallbacks is nil (not empty) when no fallbacks are configured.
	AgentModelFull(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (primary string, fallbacks []string, exists bool, err error)

	// AgentModelEntries returns cfg.agents.list[i].models — per-model-ref
	// policy keyed by the fully-qualified model ref. Entries are returned
	// as decoded generic objects so callers can merge into them without
	// dropping keys claws does not own (aliases, thinking policy, …).
	// Returns exists=false if the agent is not in agents.list[]; a present
	// agent with no models map yields an empty (non-nil) map.
	AgentModelEntries(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) (entries map[string]map[string]any, exists bool, err error)

	// AgentTools returns the tools.exec portion of
	// cfg.agents.list[idx].tools. Returns a zero-value struct when the
	// path is absent, never nil unless an error is returned.
	AgentTools(ctx context.Context, client *xssh.Client, h ConfigHost, idx int) (*RemoteToolsConfig, error)

	// Elevated returns cfg.tools.elevated. Returns a zero-value struct
	// when the path is absent, never nil unless an error is returned.
	Elevated(ctx context.Context, client *xssh.Client, h ConfigHost) (*RemoteElevatedConfig, error)

	// AgentBindings returns route bindings (type:"route") filtered by
	// agentID. Channel/Account are flattened from the wire shape's nested
	// `match` object.
	AgentBindings(ctx context.Context, client *xssh.Client, h ConfigHost, agentID string) ([]RemoteBinding, error)

	// ChannelAccounts returns channel-kind -> []accountName derived from
	// cfg.channels.<kind>.accounts. Keys match what `openclaw channels
	// list --json`'s `chat` field exposes.
	ChannelAccounts(ctx context.Context, client *xssh.Client, h ConfigHost) (map[string][]string, error)

	// DefaultAccount returns cfg.channels.<kind>.defaultAccount, or an
	// empty string when unset.
	DefaultAccount(ctx context.Context, client *xssh.Client, h ConfigHost, kind string) (string, error)

	// GatewayView returns cfg.gateway.{mode,bind} — exactly what the
	// configure-gateway step's drift check compares against the
	// manifest-derived desired values. Fields are empty when unset.
	GatewayView(ctx context.Context, client *xssh.Client, h ConfigHost) (GatewayView, error)

	// DeviceList returns the gateway's pending + paired device view.
	//
	// This does NOT live in openclaw.json; the daemon persists it to
	// two separate files (~/.openclaw/nodes/pending.json and
	// ~/.openclaw/devices/paired.json). During probe, a snapshot-backed
	// reader reads them once via SFTP and serves every subsequent call
	// from memory; outside probe the CLI impl calls
	// `openclaw devices list --json` (with the same on-disk fallback
	// for CPU-starved hosts that trip the CLI's 10 s handshake timer).
	DeviceList(ctx context.Context, client *xssh.Client, h ConfigHost) (*DeviceList, error)
}

// ConfigHost identifies the gateway node an openclaw config read is
// targeting. Used as the cache key for per-host snapshots.
type ConfigHost struct {
	Addr string
	Port int
	User string
}

// MachineConfigHost is the canonical way callers assemble a ConfigHost
// from a manifest machine + already-resolved addr. It pins the port and
// SSH user to the agent-side helpers ([MachineSSHPort], [MachineAgentUser])
// so snapshot cache keys match across every step in the phase.
func MachineConfigHost(m manifestdata.Machine, addr string) ConfigHost {
	return ConfigHost{
		Addr: addr,
		Port: MachineSSHPort(m),
		User: MachineAgentUser(m),
	}
}


// RemoteToolsConfig mirrors the subset of `agents.list[].tools` that
// ConfigureToolsStep.Check compares against the manifest. Only exec is
// surfaced today; elevated is exposed separately via [ConfigReader.Elevated]
// because openclaw stores it at the top level (cfg.tools.elevated), not
// per-agent.
type RemoteToolsConfig struct {
	Exec *RemoteExecConfig
}

// RemoteExecConfig is the shape of cfg.agents.list[].tools.exec used to
// detect drift in the configure-tools step.
type RemoteExecConfig struct {
	Host     string
	Node     string
	Security string
}

// RemoteElevatedConfig is the shape of cfg.tools.elevated used to detect
// drift in the configure-tools step. Enabled is a tri-state so callers
// can distinguish unset from false.
type RemoteElevatedConfig struct {
	Enabled   *bool
	AllowFrom map[string][]string
}

// RemoteBinding is a flattened AgentRouteBinding: the wire shape's
// {agentId, match:{channel, accountId}} is reduced to the three fields
// used by the configure-bindings step.
type RemoteBinding struct {
	AgentID string
	Channel string
	Account string
}

// GatewayView is the subset of cfg.gateway that configure-gateway's
// drift check needs. Empty strings mean the corresponding key was unset
// (or the config file didn't exist yet — fresh machine pre-bootstrap).
type GatewayView struct {
	Mode string
	Bind string
}

// DeviceList is the gateway's paired + pending device inventory. The
// CLI form (`openclaw devices list --json`) and the on-disk daemon
// files produce structurally identical lists, and both readers converge
// on this shape so callers never need to know which path served them.
type DeviceList struct {
	Pending []PendingDevice `json:"pending"`
	Paired  []PairedDevice  `json:"paired"`
}

// PendingDevice is a device awaiting gateway approval. RequestID is
// the key the daemon uses to identify this pairing attempt — passed
// verbatim to `openclaw devices approve`.
type PendingDevice struct {
	RequestID   string `json:"requestId"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

// PairedDevice is an approved device. ClientMode distinguishes
// "node" (an execution node) from "cli" (a local CLI session that
// approved itself via pair-gateway-device).
type PairedDevice struct {
	DeviceID     string   `json:"deviceId"`
	DisplayName  string   `json:"displayName"`
	ClientID     string   `json:"clientId"`
	ClientMode   string   `json:"clientMode"`
	Scopes       []string `json:"scopes"`
	LastSeenAtMs int64    `json:"lastSeenAtMs"` // 0 if node never connected
}

// HasPairedLocalDevice reports whether any CLI device is paired — the
// signal pair-gateway-device uses to determine "already satisfied".
func HasPairedLocalDevice(dl *DeviceList) bool {
	if dl == nil {
		return false
	}
	return len(dl.Paired) > 0
}

// ModelRuntimeID reads the agent-runtime id out of one
// `agents.list[].models[<ref>]` entry — i.e. `entry.agentRuntime.id`.
// Returns "" when the entry does not pin a runtime, which is also what a
// nil entry yields.
func ModelRuntimeID(entry map[string]any) string {
	rt, ok := entry["agentRuntime"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := rt["id"].(string)
	return id
}

// WithModelRuntimeID returns a copy of entry with `agentRuntime.id` set to
// id, preserving every other key — both on the entry itself and inside the
// nested agentRuntime object. openclaw model entries carry policy claws
// does not own (aliases, thinking levels, …); a runtime pin must not drop
// it. A nil entry yields a fresh one.
func WithModelRuntimeID(entry map[string]any, id string) map[string]any {
	out := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		out[k] = v
	}
	rt := make(map[string]any, 1)
	if existing, ok := entry["agentRuntime"].(map[string]any); ok {
		for k, v := range existing {
			rt[k] = v
		}
	}
	rt["id"] = id
	out["agentRuntime"] = rt
	return out
}
