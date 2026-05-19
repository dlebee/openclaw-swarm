package node

import (
	"context"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/openclawver"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

func TestPairNodeStep_Applicable_nodeTarget(t *testing.T) {
	step := NewPairNodeStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID: "scraper-node",
		Payload: &NodeTarget{
			Spec: manifestdata.Node{Name: "scraper-node"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("Applicable for *NodeTarget should be true")
	}
}

func TestPairNodeStep_Applicable_nilPayload(t *testing.T) {
	step := NewPairNodeStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{ID: "x", Payload: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("Applicable must be false for nil payload")
	}
}

func TestPairNodeStep_Applicable_wrongPayloadType(t *testing.T) {
	step := NewPairNodeStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "wrong",
		Payload: &struct{ Foo string }{Foo: "bar"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("Applicable must be false for non-NodeTarget payload")
	}
}

func TestNodeSurfaceApprovalRequiredSince_isCalverCutoff(t *testing.T) {
	want := openclawver.MustParse("2026.5.18")
	if NodeSurfaceApprovalRequiredSince != want {
		t.Fatalf("cutoff drift: got %v, want %v", NodeSurfaceApprovalRequiredSince, want)
	}

	oldCases := []string{"2024.1.0", "2026.4.24", "2026.5.2", "2026.5.17"}
	for _, in := range oldCases {
		t.Run("below/"+in, func(t *testing.T) {
			v := openclawver.MustParse(in)
			if nodeSurfaceRequired(v) {
				t.Fatalf("%s must NOT require surface approval (cutoff %s)",
					v, NodeSurfaceApprovalRequiredSince)
			}
		})
	}

	newCases := []string{"2026.5.18", "2026.5.19", "2026.6.0", "2027.1.0"}
	for _, in := range newCases {
		t.Run("atLeast/"+in, func(t *testing.T) {
			v := openclawver.MustParse(in)
			if !nodeSurfaceRequired(v) {
				t.Fatalf("%s must require surface approval (cutoff %s)",
					v, NodeSurfaceApprovalRequiredSince)
			}
		})
	}
}

func TestHasAllSurfaceCommands(t *testing.T) {
	cases := []struct {
		name     string
		commands []string
		want     []string
		ok       bool
	}{
		{
			name:     "exact match",
			commands: []string{"system.run"},
			want:     []string{"system.run"},
			ok:       true,
		},
		{
			name:     "superset",
			commands: []string{"system.run", "system.run.prepare", "system.which", "system.notify"},
			want:     []string{"system.run"},
			ok:       true,
		},
		{
			name:     "missing required",
			commands: []string{"system.notify", "browser.proxy"},
			want:     []string{"system.run"},
			ok:       false,
		},
		{
			name:     "empty commands, non-empty want",
			commands: nil,
			want:     []string{"system.run"},
			ok:       false,
		},
		{
			name:     "empty want",
			commands: []string{"anything"},
			want:     nil,
			ok:       true,
		},
		{
			name:     "both empty",
			commands: nil,
			want:     nil,
			ok:       true,
		},
		{
			name:     "multiple required, only some present",
			commands: []string{"system.run", "system.notify"},
			want:     []string{"system.run", "system.which"},
			ok:       false,
		},
		{
			name:     "multiple required all present",
			commands: []string{"system.run", "system.which", "system.run.prepare"},
			want:     []string{"system.run", "system.which"},
			ok:       true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasAllSurfaceCommands(tc.commands, tc.want)
			if got != tc.ok {
				t.Fatalf("hasAllSurfaceCommands(%v, %v) = %v, want %v",
					tc.commands, tc.want, got, tc.ok)
			}
		})
	}
}
