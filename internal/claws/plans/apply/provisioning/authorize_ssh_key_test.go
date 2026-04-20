package provisioning

import (
	"context"
	"errors"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

func TestAuthorizeSSHKeyCheck_nilDialNotSatisfied(t *testing.T) {
	a := NewAuthorizeSSHKeyStep(Options{SSHDial: nil, SSHPubKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI"})
	mt := &MachineTarget{
		Spec: manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
		Instance: &hosting.Instance{
			PublicIPv4: "203.0.113.1",
		},
	}
	satisfied, err := a.Check(context.Background(), scaffold.Target{ID: "t1", Payload: mt})
	if err != nil || satisfied {
		t.Fatalf("want not satisfied, no err; satisfied=%v err=%v", satisfied, err)
	}
}

func TestAuthorizeSSHKeyCheck_dialErrorPropagates(t *testing.T) {
	// Dial failures must propagate as errors so the probe UI can render
	// "check: dial refused" instead of lying as "will execute". See
	// .cursor/rules/plan-checks-are-pure.mdc: errors are not "unsatisfied".
	a := NewAuthorizeSSHKeyStep(Options{
		SSHDial: func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
			_ = ctx
			_ = host
			_ = port
			_ = user
			return nil, errors.New("refused")
		},
		SSHPubKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI",
	})
	mt := &MachineTarget{
		Spec: manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
		Instance: &hosting.Instance{
			PublicIPv4: "203.0.113.1",
		},
	}
	satisfied, err := a.Check(context.Background(), scaffold.Target{ID: "t1", Payload: mt})
	if err == nil || satisfied {
		t.Fatalf("want not satisfied + err; satisfied=%v err=%v", satisfied, err)
	}
}

func TestAuthorizeSSHKeyCheck_noInstanceNotSatisfied(t *testing.T) {
	a := NewAuthorizeSSHKeyStep(Options{
		SSHDial: func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
			t.Fatal("dial should not run without instance IP payload")
			return nil, nil
		},
		SSHPubKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI",
	})
	mt := &MachineTarget{
		Spec:     manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
		Instance: nil,
	}
	satisfied, err := a.Check(context.Background(), scaffold.Target{ID: "t1", Payload: mt})
	if err != nil || satisfied {
		t.Fatalf("want not satisfied, no err; satisfied=%v err=%v", satisfied, err)
	}
}
