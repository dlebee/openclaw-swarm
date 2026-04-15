package apply

import (
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

func TestBuildPlan_nilManifest(t *testing.T) {
	_, err := BuildPlan(BuildOptions{})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected error for nil manifest, got %v", err)
	}
}

func TestBuildPlan_linodeRequiresSSHDial(t *testing.T) {
	_, err := BuildPlan(BuildOptions{
		Manifest: &manifestdata.Manifest{
			Machines: []manifestdata.Machine{
				{Name: "a", Type: manifestdata.MachineTypeLinode},
			},
		},
		SSHDial: nil,
	})
	if err == nil {
		t.Fatal("expected error when Linode machines exist but SSHDial is nil")
	}
}

func TestBuildPlan_sshOnlyNoDialRequired(t *testing.T) {
	p, err := BuildPlan(BuildOptions{
		Manifest: &manifestdata.Manifest{
			Machines: []manifestdata.Machine{
				{Name: "jump", Type: manifestdata.MachineTypeSSH},
			},
		},
		SSHDial: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("expected plan")
	}
}
