package provisioning

import (
	"context"
	"fmt"
	"strings"
	"time"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/user"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

const (
	agentUserDialRetries    = 10
	agentUserDialRetryDelay = 3 * time.Second
)

// EnsureAgentUserStep creates a dedicated agent user on the target machine when
// the manifest specifies a non-root agent_user. Runs as the SSH user (root)
// after authorize-ssh-key has confirmed SSH access. Idempotent.
//
// For hosted machines (linode, multipass) the host is taken from the Instance
// record set by create-machine. For pre-provisioned SSH machines the host
// comes directly from Machine.Host — the operator is expected to have already
// authorised the claws SSH key for the bootstrap_user (root) out of band.
type EnsureAgentUserStep struct {
	dial SSHDialFunc
}

func NewEnsureAgentUserStep(opts Options) *EnsureAgentUserStep {
	return &EnsureAgentUserStep{dial: opts.SSHDial}
}

func (*EnsureAgentUserStep) Name() string { return "ensure-agent-user" }

func (s *EnsureAgentUserStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	return needsAgentUser(mt.Spec), nil
}

func (s *EnsureAgentUserStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
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

	exists, err := user.Exists(client, mt.Spec.AgentUser)
	if err != nil {
		return false, fmt.Errorf("user.Exists on %s: %w", host, err)
	}
	return exists, nil
}

func (s *EnsureAgentUserStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("ensure-agent-user: expected *MachineTarget for %q", t.ID)
	}
	host := resolveHost(mt)
	if host == "" {
		return fmt.Errorf("ensure-agent-user: no host address for %q (hosted machine not yet provisioned, or ssh machine missing host field)", t.ID)
	}
	port := sshPort(mt.Spec)
	login := bootstrapLoginUser(mt.Spec)

	var lastErr error
	for attempt := 0; attempt < agentUserDialRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		client, key, err := s.borrowSSH(ctx, host, port, login)
		if err == nil {
			err = user.Ensure(client, mt.Spec.AgentUser)
			s.returnSSH(ctx, key, client)
			if err != nil {
				return fmt.Errorf("ensure-agent-user: %w", err)
			}
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(agentUserDialRetryDelay):
		}
	}
	return fmt.Errorf("ensure-agent-user: dial %s@%s:%d after %d retries: %w", login, host, port, agentUserDialRetries, lastErr)
}

func (s *EnsureAgentUserStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("ensure-agent-user verify: expected *MachineTarget for %q", t.ID)
	}
	host := resolveHost(mt)
	if host == "" {
		return fmt.Errorf("ensure-agent-user verify: no host address for %q", t.ID)
	}
	client, key, err := s.borrowSSH(ctx, host, sshPort(mt.Spec), bootstrapLoginUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("ensure-agent-user verify: dial: %w", err)
	}
	defer s.returnSSH(ctx, key, client)

	exists, err := user.Exists(client, mt.Spec.AgentUser)
	if err != nil {
		return fmt.Errorf("ensure-agent-user verify: %w", err)
	}
	if !exists {
		return fmt.Errorf("ensure-agent-user verify: user %q does not exist", mt.Spec.AgentUser)
	}
	return nil
}

// resolveHost returns the SSH address for the machine. For hosted machines
// (linode, multipass) it uses the Instance.PublicIPv4 populated by
// create-machine; for pre-provisioned SSH machines it falls back to
// Machine.Host from the manifest.
func resolveHost(mt *MachineTarget) string {
	if mt.Instance != nil {
		if h := strings.TrimSpace(mt.Instance.PublicIPv4); h != "" {
			return h
		}
	}
	return strings.TrimSpace(mt.Spec.Host)
}

func needsAgentUser(m manifestdata.Machine) bool {
	u := strings.TrimSpace(m.AgentUser)
	return u != "" && u != "root"
}

func (s *EnsureAgentUserStep) borrowSSH(ctx context.Context, host string, port int, login string) (*xssh.Client, string, error) {
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

func (s *EnsureAgentUserStep) returnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := SSHPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}
