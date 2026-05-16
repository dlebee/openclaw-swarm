package provisioning

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/tmout"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// UnsetTMOUTStep removes the TMOUT environment variable from shell profiles on
// the target machine. Some hosting providers (e.g. OVH) ship images with TMOUT
// set system-wide, which causes non-interactive SSH sessions to be killed
// mid-script by SIGALRM (exit status 142). Enabled via options.unset_tmout in
// the manifest. Idempotent: Check reports satisfied when no TMOUT assignment
// remains in the profiles we manage.
type UnsetTMOUTStep struct {
	dial SSHDialFunc
}

// NewUnsetTMOUTStep builds the step from provisioning options.
func NewUnsetTMOUTStep(opts Options) *UnsetTMOUTStep {
	return &UnsetTMOUTStep{dial: opts.SSHDial}
}

func (*UnsetTMOUTStep) Name() string { return "unset-tmout" }

func (s *UnsetTMOUTStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*MachineTarget)
	return ok, nil
}

func (s *UnsetTMOUTStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	host := resolveHost(mt)
	if host == "" {
		return false, nil
	}
	client, key, err := s.borrowSSH(ctx, host, sshPort(mt.Spec), bootstrapLoginUser(mt.Spec))
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", host, err)
	}
	defer s.returnSSH(ctx, key, client)

	isSet, err := tmout.IsSet(client)
	if err != nil {
		return false, fmt.Errorf("unset-tmout check on %s: %w", host, err)
	}
	return !isSet, nil
}

func (s *UnsetTMOUTStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("unset-tmout: expected *MachineTarget for %q", t.ID)
	}
	host := resolveHost(mt)
	if host == "" {
		return fmt.Errorf("unset-tmout: no host address for %q", t.ID)
	}
	client, key, err := s.borrowSSH(ctx, host, sshPort(mt.Spec), bootstrapLoginUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("unset-tmout: dial %s: %w", host, err)
	}
	defer s.returnSSH(ctx, key, client)

	if err := tmout.Unset(client); err != nil {
		return fmt.Errorf("unset-tmout: %w", err)
	}
	return nil
}

func (s *UnsetTMOUTStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("unset-tmout verify: expected *MachineTarget for %q", t.ID)
	}
	host := resolveHost(mt)
	if host == "" {
		return fmt.Errorf("unset-tmout verify: no host address for %q", t.ID)
	}
	client, key, err := s.borrowSSH(ctx, host, sshPort(mt.Spec), bootstrapLoginUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("unset-tmout verify: dial: %w", err)
	}
	defer s.returnSSH(ctx, key, client)

	isSet, err := tmout.IsSet(client)
	if err != nil {
		return fmt.Errorf("unset-tmout verify: %w", err)
	}
	if isSet {
		return fmt.Errorf("unset-tmout verify: TMOUT still present in shell profiles on %s", host)
	}
	return nil
}

func (s *UnsetTMOUTStep) borrowSSH(ctx context.Context, host string, port int, login string) (*xssh.Client, string, error) {
	key := clawssh.HostKey(host, port, login)
	if pool := SSHPool(ctx); pool != nil {
		dial := s.dial
		c, err := pool.Borrow(ctx, key, func(ctx context.Context) (*xssh.Client, error) {
			return dial(ctx, host, port, login)
		})
		return c, key, err
	}
	c, err := s.dial(ctx, host, port, login)
	return c, key, err
}

func (s *UnsetTMOUTStep) returnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := SSHPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}
