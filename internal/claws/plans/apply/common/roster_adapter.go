package common

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	xssh "golang.org/x/crypto/ssh"
)

// ---------------------------------------------------------------------------
// Version-agnostic agent-roster DTO
// ---------------------------------------------------------------------------
//
// OpenClaw reshaped its agent roster between 2026.7.x and 2026.8.1:
//
//	2026.7.x:  agents.list   = [ {id, model, models, tools}, ... ]   (array)
//	2026.8.1:  agents.entries = { <id>: {model, models, tools} }     (map)
//
// The read side is asymmetric and unforgiving: on 2026.8.1 `config get
// agents.list` is a hard error, and on 2026.7.x `config get agents.entries`
// is a hard error. Writes are worse — the indexed `agents.list[i].*` paths
// are a documented-legacy shim on 2026.8.1 that mangles model refs and drops
// exec keys, so writing through them silently corrupts the roster.
//
// Rather than smear `if version...` branches across every read/write/verify
// site, claws speaks ONE internal DTO (AgentRoster / AgentEntry) and pushes
// all shape/path knowledge behind a RosterAdapter. Each supported CLI major
// gets one adapter; DetectRosterAdapter picks the right one per host, once.

// AgentEntry is the version-agnostic view of a single agent. Model is kept
// as raw JSON because openclaw accepts either a bare string or a
// {primary, fallbacks} object; callers use ModelPrimary/ModelFallbacks (or
// the extract* helpers) to normalise. Models is the per-model-ref policy
// map, decoded generically so runtime pins merge without dropping sibling
// keys claws does not own.
type AgentEntry struct {
	ID        string
	Model     json.RawMessage
	Models    map[string]map[string]any
	ExecTools *RemoteExecConfig
}

// ModelPrimary returns the primary model ref for this entry (string or
// object form), or "" when unset.
func (a AgentEntry) ModelPrimary() string { return extractModelPrimary(a.Model) }

// ModelFallbacks returns the fallback model refs, or nil when none.
func (a AgentEntry) ModelFallbacks() []string { return extractModelFallbacks(a.Model) }

// AgentRoster is the whole roster plus the defaults block, independent of
// on-disk shape. Order is stable (list order on 7.x; id-sorted on 8.1).
type AgentRoster struct {
	Agents  []AgentEntry
	Defaults struct {
		Model json.RawMessage
	}
}

// IndexOf returns the position of agentID (case-insensitive) or -1.
func (r AgentRoster) IndexOf(agentID string) int {
	target := normalizeAgentID(agentID)
	for i, a := range r.Agents {
		if normalizeAgentID(a.ID) == target {
			return i
		}
	}
	return -1
}

// Find returns the entry for agentID (case-insensitive) and whether it
// exists.
func (r AgentRoster) Find(agentID string) (AgentEntry, bool) {
	if i := r.IndexOf(agentID); i >= 0 {
		return r.Agents[i], true
	}
	return AgentEntry{}, false
}

// ---------------------------------------------------------------------------
// Adapter interface
// ---------------------------------------------------------------------------

// RosterAdapter translates between claws' internal AgentRoster DTO and the
// concrete on-disk / CLI config shape of one OpenClaw major version. All
// read AND write paths for the agent roster go through an adapter so the
// version boundary lives in exactly one place.
type RosterAdapter interface {
	// Kind identifies the adapter for logging/tests, e.g. "v7"/"v8".
	Kind() string

	// ParseRoster decodes a raw ~/.openclaw/openclaw.json agents block into
	// the DTO. Used by the snapshot (SFTP) reader. A missing/empty block
	// yields an empty roster, never an error — a fresh gateway has no
	// agents yet and that must not read as failure.
	ParseRoster(agentsBlock json.RawMessage) (AgentRoster, error)

	// RosterReadScript returns the shell that prints the roster JSON this
	// adapter's ParseRosterCLI can decode. It must succeed (exit 0, empty
	// object) on a fresh gateway rather than erroring on an absent path.
	RosterReadScript() string

	// ParseRosterCLI decodes the stdout of RosterReadScript.
	ParseRosterCLI(raw string) (AgentRoster, error)

	// ModelWritePath returns the config path + value for the agent's model
	// field. On 8.1 the value is always the object form (primary lives at
	// model.primary and a bare-string write of an unresolved ref is
	// rejected); on 7.x either form is accepted.
	ModelWrite(idx int, id, primary string, fallbacks []string) (path string, value any)

	// ModelEntriesWritePath returns the config path for the per-model-ref
	// policy map (agents.<shape>.models).
	ModelEntriesWritePath(idx int, id string) string

	// ExecWritePaths returns the individual tools.exec.* config paths to set
	// for this agent. Split rather than one object so an unset field in the
	// manifest doesn't clobber an existing value.
	ExecWritePaths(idx int, id string) (hostPath, nodePath, securityPath string)

	// SeedAgentWrite returns the config path + value that creates the agent
	// entry from scratch (used for the reserved "main" id, which `agents
	// add` refuses). On 7.x this seeds agents.list; on 8.1 agents.entries.
	SeedAgentWrite(id, workspace string) (path string, value any)
}

// ---------------------------------------------------------------------------
// Version detection
// ---------------------------------------------------------------------------

// rosterAdapterForVersion maps a parsed major.minor to an adapter. The
// roster reshape landed in 2026.8.0 (named-agent setup); anything >= that
// speaks entries, everything older speaks list.
func rosterAdapterForVersion(year, minor int) RosterAdapter {
	if year > 2026 || (year == 2026 && minor >= 8) {
		return v8RosterAdapter{}
	}
	return v7RosterAdapter{}
}

// parseOpenclawVersion pulls "2026.8.1" out of `openclaw --version` output
// and returns (year, minor, ok). Tolerates leading "v", surrounding
// whitespace, and trailing prerelease/build noise.
func parseOpenclawVersion(s string) (year, minor int, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	// Grab the first whitespace-delimited token that looks like a version.
	for _, tok := range strings.Fields(s) {
		tok = strings.TrimPrefix(tok, "v")
		parts := strings.SplitN(tok, ".", 3)
		if len(parts) < 2 {
			continue
		}
		y, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && y >= 2000 {
			return y, m, true
		}
	}
	return 0, 0, false
}

// DetectRosterAdapter probes `openclaw --version` on the host and returns
// the matching adapter. When the version can't be parsed it falls back to a
// schema probe: `config get agents.entries` succeeds on 8.1 and errors on
// 7.x, which disambiguates without conflating "unknown path" with "empty
// config" (the probe only runs when the version string is unusable).
func DetectRosterAdapter(client *xssh.Client) (RosterAdapter, error) {
	out, err := bash.RunOutput(client, OpenclawCLIPreamble()+`openclaw --version 2>/dev/null || echo ""`)
	if err == nil {
		if y, m, ok := parseOpenclawVersion(out); ok {
			return rosterAdapterForVersion(y, m), nil
		}
	}
	// Version string unusable — fall back to a schema probe. Distinguish
	// "path exists" (8.1) from a hard "unknown path" error (7.x). We ask
	// the CLI to print a sentinel on failure so we can tell the two apart.
	probe := OpenclawCLIPreamble() +
		`openclaw config get agents.entries --json 2>/dev/null && echo "__OK__" || echo "__ERR__"`
	pout, perr := bash.RunOutput(client, probe)
	if perr != nil {
		return nil, fmt.Errorf("detect roster adapter: version + schema probe both failed: %w", perr)
	}
	if strings.Contains(pout, "__OK__") {
		return v8RosterAdapter{}, nil
	}
	return v7RosterAdapter{}, nil
}

// ---------------------------------------------------------------------------
// v7 adapter — agents.list (array, index paths)
// ---------------------------------------------------------------------------

type v7RosterAdapter struct{}

func (v7RosterAdapter) Kind() string { return "v7" }

// v7/v8 share one on-disk agent value shape; only the container differs.
type rawAgentValue struct {
	ID     string                    `json:"id"`
	Model  json.RawMessage           `json:"model"`
	Models map[string]map[string]any `json:"models"`
	Tools  *struct {
		Exec *RemoteExecConfig `json:"exec"`
	} `json:"tools"`
}

func (rv rawAgentValue) toEntry(fallbackID string) AgentEntry {
	id := strings.TrimSpace(rv.ID)
	if id == "" {
		id = fallbackID
	}
	e := AgentEntry{ID: id, Model: rv.Model, Models: rv.Models}
	if rv.Tools != nil {
		e.ExecTools = rv.Tools.Exec
	}
	return e
}

// v7AgentsBlock is the shape of the `agents` object on 2026.7.x.
type v7AgentsBlock struct {
	List     []rawAgentValue `json:"list"`
	Defaults struct {
		Model json.RawMessage `json:"model"`
	} `json:"defaults"`
}

func (v7RosterAdapter) ParseRoster(block json.RawMessage) (AgentRoster, error) {
	var b v7AgentsBlock
	if len(strings.TrimSpace(string(block))) > 0 {
		if err := json.Unmarshal(block, &b); err != nil {
			return AgentRoster{}, fmt.Errorf("v7 parse agents block: %w", err)
		}
	}
	r := AgentRoster{}
	r.Defaults.Model = b.Defaults.Model
	for _, rv := range b.List {
		r.Agents = append(r.Agents, rv.toEntry(""))
	}
	return r, nil
}

// On 7.x, `config get agents.list` is the roster; a fresh gateway has no
// agents key, so `|| echo "[]"` supplies the empty case WITHOUT masking a
// genuine failure (a real error still prints its envelope, but the array we
// then parse is empty — acceptable because 7.x has no "unknown path" trap
// for agents.list the way 8.1 does).
func (v7RosterAdapter) RosterReadScript() string {
	return `openclaw config get agents.list --json 2>/dev/null || echo "[]"`
}

func (v7RosterAdapter) ParseRosterCLI(raw string) (AgentRoster, error) {
	raw = extractJSON(strings.TrimSpace(raw), '[')
	var list []rawAgentValue
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return AgentRoster{}, fmt.Errorf("v7 parse agents.list %q: %w", raw, err)
	}
	r := AgentRoster{}
	for _, rv := range list {
		r.Agents = append(r.Agents, rv.toEntry(""))
	}
	return r, nil
}

func (v7RosterAdapter) ModelWrite(idx int, _ string, primary string, fallbacks []string) (string, any) {
	path := fmt.Sprintf("agents.list[%d].model", idx)
	if len(fallbacks) > 0 {
		return path, map[string]any{"primary": primary, "fallbacks": fallbacks}
	}
	return path, primary
}

func (v7RosterAdapter) ModelEntriesWritePath(idx int, _ string) string {
	return fmt.Sprintf("agents.list[%d].models", idx)
}

func (v7RosterAdapter) ExecWritePaths(idx int, _ string) (string, string, string) {
	p := fmt.Sprintf("agents.list[%d].tools.exec", idx)
	return p + ".host", p + ".node", p + ".security"
}

func (v7RosterAdapter) SeedAgentWrite(id, workspace string) (string, any) {
	entry := map[string]any{"id": id}
	if workspace != "" {
		entry["workspace"] = workspace
	}
	return "agents.list", []map[string]any{entry}
}

// ---------------------------------------------------------------------------
// v8 adapter — agents.entries (map, id paths)
// ---------------------------------------------------------------------------

type v8RosterAdapter struct{}

func (v8RosterAdapter) Kind() string { return "v8" }

// v8AgentsBlock is the shape of the `agents` object on 2026.8.1. The id is
// the map key, not a field on the value.
type v8AgentsBlock struct {
	Entries  map[string]rawAgentValue `json:"entries"`
	Defaults struct {
		Model json.RawMessage `json:"model"`
	} `json:"defaults"`
}

func (v8RosterAdapter) ParseRoster(block json.RawMessage) (AgentRoster, error) {
	var b v8AgentsBlock
	if len(strings.TrimSpace(string(block))) > 0 {
		if err := json.Unmarshal(block, &b); err != nil {
			return AgentRoster{}, fmt.Errorf("v8 parse agents block: %w", err)
		}
	}
	r := AgentRoster{}
	r.Defaults.Model = b.Defaults.Model
	r.Agents = entriesToSortedSlice(b.Entries)
	return r, nil
}

func (v8RosterAdapter) RosterReadScript() string {
	// On 8.1 a fresh gateway with no agents still resolves agents.entries to
	// {} (the key exists in the schema), so no `|| echo` fallback is needed
	// here — but we keep one for the pre-agents-key window right after
	// install, printing an empty object rather than erroring.
	return `openclaw config get agents.entries --json 2>/dev/null || echo "{}"`
}

func (v8RosterAdapter) ParseRosterCLI(raw string) (AgentRoster, error) {
	raw = extractJSON(strings.TrimSpace(raw), '{')
	var entries map[string]rawAgentValue
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return AgentRoster{}, fmt.Errorf("v8 parse agents.entries %q: %w", raw, err)
	}
	return AgentRoster{Agents: entriesToSortedSlice(entries)}, nil
}

func (v8RosterAdapter) ModelWrite(_ int, id string, primary string, fallbacks []string) (string, any) {
	// On 8.1 the model MUST be written as the object form: a bare-string
	// write of an authored ref at model.primary is what triggered
	// "Unable to resolve authored model reference". Writing the object at
	// agents.entries.<id>.model keeps primary+fallbacks atomic and lets the
	// CLI resolve the ref the same way it does for the object shape.
	path := fmt.Sprintf("agents.entries.%s.model", id)
	val := map[string]any{"primary": primary}
	if len(fallbacks) > 0 {
		val["fallbacks"] = fallbacks
	}
	return path, val
}

func (v8RosterAdapter) ModelEntriesWritePath(_ int, id string) string {
	return fmt.Sprintf("agents.entries.%s.models", id)
}

func (v8RosterAdapter) ExecWritePaths(_ int, id string) (string, string, string) {
	p := fmt.Sprintf("agents.entries.%s.tools.exec", id)
	return p + ".host", p + ".node", p + ".security"
}

func (v8RosterAdapter) SeedAgentWrite(id, workspace string) (string, any) {
	// id is the map key; the value carries no id field on 8.1.
	val := map[string]any{}
	if workspace != "" {
		val["workspace"] = workspace
	}
	return fmt.Sprintf("agents.entries.%s", id), val
}

// entriesToSortedSlice folds the map key into each entry's ID and returns a
// slice ordered by id, so index positions are stable within one read.
func entriesToSortedSlice(entries map[string]rawAgentValue) []AgentEntry {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AgentEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, entries[id].toEntry(id))
	}
	return out
}
