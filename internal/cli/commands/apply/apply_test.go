package apply

import (
	"strings"
	"testing"
)

func TestNormalizePhaseList(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"trims+drops_empty", []string{"  provisioning ", "", " security"}, []string{"provisioning", "security"}},
		{"all_blank_reduces_to_nil", []string{"", "   "}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePhaseList(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, got)
				}
			}
		})
	}
}

func TestValidatePhaseNames(t *testing.T) {
	available := []string{"provisioning", "security", "mesh-join", "gateway", "node"}

	// Empty request is always OK (= "no filter").
	if err := validatePhaseNames("phases", nil, available); err != nil {
		t.Fatalf("empty request should pass: %v", err)
	}

	// All known names pass.
	if err := validatePhaseNames("phases", []string{"provisioning", "gateway"}, available); err != nil {
		t.Fatalf("known names should pass: %v", err)
	}

	// Unknown names are rejected and the message lists both the typo and the
	// real set — we want users to copy/paste a correction, not re-run --help.
	err := validatePhaseNames("skip-phases", []string{"gatewayy", "node"}, available)
	if err == nil {
		t.Fatal("expected error for unknown phase name")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--skip-phases") {
		t.Fatalf("error should mention the flag name: %q", msg)
	}
	if !strings.Contains(msg, "gatewayy") {
		t.Fatalf("error should name the bad entry: %q", msg)
	}
	if !strings.Contains(msg, "available:") || !strings.Contains(msg, "gateway") {
		t.Fatalf("error should list available phases: %q", msg)
	}
}
