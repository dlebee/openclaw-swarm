package agents

import (
	"reflect"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

func TestModelRuntimeDrift(t *testing.T) {
	t.Parallel()

	pinned := func(id string) map[string]any {
		return map[string]any{"agentRuntime": map[string]any{"id": id}}
	}

	tests := []struct {
		name    string
		desired manifestdata.AgentModels
		remote  map[string]map[string]any
		want    []string
	}{
		{
			name:    "converged",
			desired: manifestdata.AgentModels{"anthropic/claude-opus-5": {Runtime: "claude-cli"}},
			remote:  map[string]map[string]any{"anthropic/claude-opus-5": pinned("claude-cli")},
		},
		{
			name:    "missing pin",
			desired: manifestdata.AgentModels{"anthropic/claude-opus-5": {Runtime: "claude-cli"}},
			remote:  map[string]map[string]any{},
			want:    []string{"anthropic/claude-opus-5"},
		},
		{
			name:    "wrong runtime",
			desired: manifestdata.AgentModels{"anthropic/claude-opus-5": {Runtime: "claude-cli"}},
			remote:  map[string]map[string]any{"anthropic/claude-opus-5": pinned("openclaw")},
			want:    []string{"anthropic/claude-opus-5"},
		},
		{
			name: "drift is per model, not per agent",
			desired: manifestdata.AgentModels{
				"anthropic/claude-opus-5":   {Runtime: "claude-cli"},
				"anthropic/claude-sonnet-5": {Runtime: "claude-cli"},
			},
			remote: map[string]map[string]any{"anthropic/claude-opus-5": pinned("claude-cli")},
			want:   []string{"anthropic/claude-sonnet-5"},
		},
		{
			name:    "remote-only refs are not claws-owned",
			desired: manifestdata.AgentModels{"anthropic/claude-opus-5": {Runtime: "claude-cli"}},
			remote: map[string]map[string]any{
				"anthropic/claude-opus-5": pinned("claude-cli"),
				"openai/gpt-5.5":          pinned("codex"),
			},
		},
		{
			name:    "entry without a runtime is not drift",
			desired: manifestdata.AgentModels{"anthropic/claude-opus-5": {Runtime: "  "}},
			remote:  map[string]map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := modelRuntimeDrift(tt.desired, tt.remote)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("drift: got %v, want %v", got, tt.want)
			}
		})
	}
}
