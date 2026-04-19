package provisioning

import (
	"context"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

func TestWaitCloudInit_Name(t *testing.T) {
	step := NewWaitCloudInitStep(Options{})
	if step.Name() != "wait-cloud-init" {
		t.Fatalf("name = %q, want wait-cloud-init", step.Name())
	}
}

func TestWaitCloudInit_NotApplicable_nilDial(t *testing.T) {
	// Without an SSH dialer we cannot probe the host; the step is a
	// no-op in that configuration. Matches authorize-ssh-key's stance.
	step := NewWaitCloudInitStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "web",
		Payload: &MachineTarget{Spec: manifestdata.Machine{Type: manifestdata.MachineTypeLinode}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be applicable when SSHDial is nil")
	}
}

func TestWaitCloudInit_NotApplicable_ssh(t *testing.T) {
	// SSH-typed machines are pre-provisioned: they are past their own
	// first-boot window by the time claws sees them, so there is no
	// cloud-init dance to wait on.
	step := NewWaitCloudInitStep(Options{SSHDial: noopDial})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "jump",
		Payload: &MachineTarget{Spec: manifestdata.Machine{Type: manifestdata.MachineTypeSSH}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be applicable to SSH machines")
	}
}

func TestWaitCloudInit_Applicable_hostedWithDialer(t *testing.T) {
	step := NewWaitCloudInitStep(Options{SSHDial: noopDial})
	for _, typ := range []manifestdata.MachineType{
		manifestdata.MachineTypeLinode,
		manifestdata.MachineTypeMultipass,
	} {
		ok, err := step.Applicable(context.Background(), scaffold.Target{
			ID:      "web",
			Payload: &MachineTarget{Spec: manifestdata.Machine{Type: typ}},
		})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !ok {
			t.Fatalf("expected applicable for hosted type %q", typ)
		}
	}
}

// noopDial is a placeholder SSHDialFunc for Applicable tests — it is never
// actually invoked because Applicable does not open a connection.
func noopDial(_ context.Context, _ string, _ int, _ string) (*xssh.Client, error) {
	return nil, nil
}
