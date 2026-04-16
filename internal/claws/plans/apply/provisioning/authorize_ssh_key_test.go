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

func TestAuthorizeSSHKeyCheck_nilDialNotBlocked(t *testing.T) {
	a := NewAuthorizeSSHKeyAction(Options{SSHDial: nil, SSHPubKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI"})
	mt := &MachineTarget{
		Spec: manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
		Instance: &hosting.Instance{
			PublicIPv4: "203.0.113.1",
		},
	}
	blocked, err := a.Check(context.Background(), scaffold.Target{ID: "t1", Payload: mt})
	if err != nil || blocked {
		t.Fatalf("want not blocked, no err; blocked=%v err=%v", blocked, err)
	}
}

func TestAuthorizeSSHKeyCheck_dialErrorNotBlocked(t *testing.T) {
	a := NewAuthorizeSSHKeyAction(Options{
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
	blocked, err := a.Check(context.Background(), scaffold.Target{ID: "t1", Payload: mt})
	if err != nil || blocked {
		t.Fatalf("want not blocked, no err; blocked=%v err=%v", blocked, err)
	}
}

func TestAuthorizeSSHKeyCheck_noInstanceNotBlocked(t *testing.T) {
	a := NewAuthorizeSSHKeyAction(Options{
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
	blocked, err := a.Check(context.Background(), scaffold.Target{ID: "t1", Payload: mt})
	if err != nil || blocked {
		t.Fatalf("want not blocked, no err; blocked=%v err=%v", blocked, err)
	}
}
