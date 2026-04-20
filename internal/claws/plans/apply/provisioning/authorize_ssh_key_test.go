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

func TestAuthorizeSSHKeyCheck_dialErrorYieldsUnsatisfied(t *testing.T) {
	// Dial/auth failures must NOT propagate as errors from Check — they mean
	// "can't verify yet, Execute will retry with backoff". Returning (false, err)
	// would abort the execution-phase cell before Execute's retry loop runs,
	// which is exactly what caused the Linode SSH-timeout regressions.
	// The probe UI shows "will execute" for (false, nil), which is the correct
	// semantics: we don't know whether the key is present, so Execute must run.
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
	if err != nil || satisfied {
		t.Fatalf("want (false, nil) on dial error; satisfied=%v err=%v", satisfied, err)
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
