package manifestcmd

import (
	"strings"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

func TestRenderManifest_containsSections(t *testing.T) {
	m := &data.Manifest{
		Prefix:    "lab",
		NodeMajor: 22,
		Machines: []data.Machine{
			{Name: "m1", Type: data.MachineTypeSSH, Region: "us", SSHUser: "u", AgentUser: "a"},
		},
		Gateways: []data.Gateway{{Name: "gw1", Reference: "m1"}},
	}
	out := RenderManifest("/tmp/x.yml", m, 100)
	if !strings.Contains(out, "lab") || !strings.Contains(out, "m1") || !strings.Contains(out, "gw1") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if !strings.Contains(out, "22") {
		t.Fatalf("expected node_major 22 in output:\n%s", out)
	}
}

func TestOptionalInt(t *testing.T) {
	if !strings.Contains(optionalInt(0), "—") {
		t.Fatalf("zero: %q", optionalInt(0))
	}
	if optionalInt(22) != "22" {
		t.Fatalf("non-zero: %q", optionalInt(22))
	}
}
