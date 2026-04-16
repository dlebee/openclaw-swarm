package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	xssh "golang.org/x/crypto/ssh"
)

// AgentInfo is one entry from `openclaw agents list --json`.
type AgentInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"`
	Bindings  int    `json:"bindings"`
	IsDefault bool   `json:"isDefault"`
}

// ListAgents runs `openclaw agents list --json` and parses the result.
func ListAgents(client *xssh.Client) ([]AgentInfo, error) {
	out, err := bash.RunOutput(client, `openclaw agents list --json 2>/dev/null || echo "[]"`)
	if err != nil {
		return nil, fmt.Errorf("agents list: %w", err)
	}
	raw := extractJSON(strings.TrimSpace(out), '[')
	var agents []AgentInfo
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		return nil, fmt.Errorf("agents list: parse %q: %w", raw, err)
	}
	return agents, nil
}

// FindAgent returns the AgentInfo for the given ID, or nil if not found.
func FindAgent(client *xssh.Client, id string) (*AgentInfo, error) {
	agents, err := ListAgents(client)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.ID == id {
			return &a, nil
		}
	}
	return nil, nil
}

// AgentConfigIndex returns the 0-based index of the agent in agents.list[].
// Returns -1 if not found. Uses `openclaw config get agents.list` to read
// the raw list and find the position by ID.
func AgentConfigIndex(client *xssh.Client, id string) (int, error) {
	out, err := bash.RunOutput(client, `openclaw config get agents.list --json 2>/dev/null || echo "[]"`)
	if err != nil {
		return -1, fmt.Errorf("agent config index: %w", err)
	}
	raw := extractJSON(strings.TrimSpace(out), '[')
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return -1, nil
	}
	for i, entry := range list {
		if entry.ID == id {
			return i, nil
		}
	}
	return -1, nil
}

// BindingInfo is one entry from `openclaw agents bindings --json`.
type BindingInfo struct {
	AgentID string `json:"agentId"`
	Channel string `json:"channel"`
	Account string `json:"accountId"`
}

// ListBindings runs `openclaw agents bindings --agent <id> --json` and parses the result.
func ListBindings(client *xssh.Client, agentID string) ([]BindingInfo, error) {
	out, err := bash.RunOutput(client, fmt.Sprintf(
		`openclaw agents bindings --agent %q --json 2>/dev/null || echo "[]"`, agentID))
	if err != nil {
		return nil, fmt.Errorf("agent bindings: %w", err)
	}
	raw := extractJSON(strings.TrimSpace(out), '[')
	var bindings []BindingInfo
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		return nil, nil
	}
	return bindings, nil
}

// extractJSON finds the first occurrence of startChar ('{' or '[') in the
// output and returns from there onward. Handles CLI commands that emit
// non-JSON preamble (daemon connection messages, etc.) before the JSON payload.
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
