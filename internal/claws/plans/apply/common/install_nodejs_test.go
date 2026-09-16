package common

import (
	"strings"
	"testing"
)

func TestParseNodeVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    NodeVersion
		wantErr bool
	}{
		{in: "v24.21.0", want: NodeVersion{24, 21, 0}},
		{in: "  v22.23.2\n", want: NodeVersion{22, 23, 2}},
		{in: "v26.1.10", want: NodeVersion{26, 1, 10}},
		{in: "missing", wantErr: true},
		{in: "", wantErr: true},
		{in: "24.21.0", wantErr: true}, // node always prints the leading v
	}
	for _, tc := range cases {
		got, err := ParseNodeVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseNodeVersion(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseNodeVersion(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestNodeVersionSatisfies(t *testing.T) {
	cases := []struct {
		have      NodeVersion
		wantMajor int
		want      bool
	}{
		// The OpenClaw 2026.9.3 failure: a host still on Node 22.
		{NodeVersion{22, 23, 2}, 24, false},
		{NodeVersion{24, 21, 0}, 24, true},
		{NodeVersion{24, 16, 0}, 24, true},
		// Right major, below OpenClaw's floor for it.
		{NodeVersion{24, 15, 9}, 24, false},
		// Newer major than wanted is still a mismatch (apt won't downgrade;
		// Verify reports it).
		{NodeVersion{26, 8, 2}, 24, false},
		// Explicit node_major: 22 override honours the 22.x floor.
		{NodeVersion{22, 23, 2}, 22, true},
		{NodeVersion{22, 22, 2}, 22, false},
		// Majors without a floor only need to match.
		{NodeVersion{25, 0, 0}, 25, true},
	}
	for _, tc := range cases {
		if got := nodeVersionSatisfies(tc.have, tc.wantMajor); got != tc.want {
			t.Errorf("nodeVersionSatisfies(%v, %d) = %v, want %v", tc.have, tc.wantMajor, got, tc.want)
		}
	}
}

func TestResolveNodeMajor(t *testing.T) {
	if got := ResolveNodeMajor(0); got != DefaultNodeMajor {
		t.Errorf("unset node_major: got %d, want DefaultNodeMajor %d", got, DefaultNodeMajor)
	}
	if got := ResolveNodeMajor(26); got != 26 {
		t.Errorf("node_major override: got %d, want 26", got)
	}
}

func TestNewInstallNodejsStep_nodeMajor(t *testing.T) {
	if got := NewInstallNodejsStep(Options{}).nodeMajor; got != DefaultNodeMajor {
		t.Errorf("default step major = %d, want %d", got, DefaultNodeMajor)
	}
	if got := NewInstallNodejsStep(Options{NodeMajor: 26}).nodeMajor; got != 26 {
		t.Errorf("override step major = %d, want 26", got)
	}
}

func TestDefaultNodeMajorClearsOpenclawFloor(t *testing.T) {
	// OpenClaw 2026.9.x engines: node >=24.16.0 <25 || >=26.1.0. The default
	// line must have a floor entry so stale 24.x hosts get upgraded.
	floor, ok := minNodeVersions[DefaultNodeMajor]
	if !ok {
		t.Fatalf("minNodeVersions has no floor for DefaultNodeMajor %d", DefaultNodeMajor)
	}
	if !strings.HasPrefix(floor.String(), "v24.") && !strings.HasPrefix(floor.String(), "v26.") {
		t.Errorf("DefaultNodeMajor floor %s is outside OpenClaw's supported lines", floor)
	}
}
