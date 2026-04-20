package automations

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// remoteUnreachableTarget returns an AutomationTarget whose Host() is ""
// (machine not yet provisioned). Check() uses this to short-circuit so we
// never try to open an SSH client in a unit test.
func remoteUnreachableTarget() scaffold.Target {
	return scaffold.Target{
		ID: "node-host",
		Payload: &AutomationTarget{MachineTarget: &provisioning.MachineTarget{
			Spec: manifestdata.Machine{
				Name: "node-host",
				Type: manifestdata.MachineTypeSSH,
			},
		}},
	}
}

// TestDynamicStep_IfChanged_UnreachableShortCircuits asserts the same
// host-empty short-circuit that applies to every other Check path also
// fires when if_changed is enabled — otherwise the probe would try to
// sha256sum a host that doesn't exist yet during `claws apply`.
func TestDynamicStep_IfChanged_UnreachableShortCircuits(t *testing.T) {
	t.Parallel()
	d := NewDynamicStep(manifestdata.AutomationStep{
		Name:        "push",
		Kind:        manifestdata.StepKindSCPUpload,
		Source:      "/nonexistent/source",
		Destination: "/remote/path",
		IfChanged:   true,
	}, "", nil, Options{})

	ok, err := d.Check(context.Background(), remoteUnreachableTarget())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("unreachable target should not be satisfied")
	}
}

// TestDynamicStep_IfChanged_LocalMissingPropagates verifies that when
// if_changed is set and the local source doesn't exist, the Check
// surfaces the error rather than silently reporting "not satisfied".
// That's the cache-purity rule: errors are not cache misses.
func TestDynamicStep_IfChanged_LocalMissingPropagates(t *testing.T) {
	t.Parallel()
	// Target pretending to be reachable so we bypass the host-empty
	// short-circuit and exercise the local hash branch. We never reach
	// the SSH dial because LocalSHA256 fails first.
	tgt := scaffold.Target{
		ID: "node-host",
		Payload: &AutomationTarget{MachineTarget: &provisioning.MachineTarget{
			Spec: manifestdata.Machine{
				Name: "node-host",
				Type: manifestdata.MachineTypeSSH,
				Host: "127.0.0.1",
			},
			Instance: nil,
		}},
	}
	d := NewDynamicStep(manifestdata.AutomationStep{
		Name:        "push",
		Kind:        manifestdata.StepKindSCPUpload,
		Source:      filepath.Join(t.TempDir(), "missing.yaml"),
		Destination: "/remote/path",
		IfChanged:   true,
	}, "", nil, Options{})

	_, err := d.Check(context.Background(), tgt)
	if err == nil {
		t.Fatalf("want error for missing local source, got nil")
	}
	if !strings.Contains(err.Error(), "hash local") {
		t.Fatalf("want wrapped hash-local error, got %v", err)
	}
}

// TestDynamicStep_ResolveSourcePath makes sure the new helper joins
// relative source paths against ManifestDir (matching the *_file script
// resolver contract) and leaves absolutes alone.
func TestDynamicStep_ResolveSourcePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a file so callers that actually hash the result find it;
	// the helper itself doesn't care, but we exercise the integration.
	payload := []byte("x")
	if err := os.WriteFile(filepath.Join(dir, "rel.yaml"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		source      string
		manifestDir string
		want        string
	}{
		{"absolute ignores manifestdir", "/abs/path", "/whatever", "/abs/path"},
		{"relative joined against manifestdir", "rel.yaml", dir, filepath.Join(dir, "rel.yaml")},
		{"relative with empty manifestdir passes through", "rel.yaml", "", "rel.yaml"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewDynamicStep(manifestdata.AutomationStep{
				Name:        "push",
				Kind:        manifestdata.StepKindSCPUpload,
				Source:      tc.source,
				Destination: "/remote/path",
			}, "", nil, Options{ManifestDir: tc.manifestDir})
			if got := d.resolveSourcePath(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
