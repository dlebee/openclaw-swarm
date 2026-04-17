package mesh

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

func sshPool(ctx context.Context) *clawssh.Pool {
	return provisioning.SSHPool(ctx)
}

const (
	sshDialRetries    = 10
	sshDialRetryDelay = 3 * time.Second
)

func borrowSSH(ctx context.Context, dial SSHDialFunc, host string, port int, user string) (*xssh.Client, string, error) {
	key := clawssh.HostKey(host, port, user)
	if pool := sshPool(ctx); pool != nil {
		c, err := pool.Borrow(ctx, key, func(ctx context.Context) (*xssh.Client, error) {
			return dial(ctx, host, port, user)
		})
		return c, key, err
	}
	c, err := dial(ctx, host, port, user)
	return c, key, err
}

func borrowSSHWithRetry(ctx context.Context, dial SSHDialFunc, host string, port int, user string) (*xssh.Client, string, error) {
	var lastErr error
	for attempt := 0; attempt < sshDialRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		c, key, err := borrowSSH(ctx, dial, host, port, user)
		if err == nil {
			return c, key, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(sshDialRetryDelay):
		}
	}
	return nil, "", fmt.Errorf("dial %s@%s:%d after %d retries: %w", user, host, port, sshDialRetries, lastErr)
}

func returnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := sshPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}

// machineHost resolves the reachable host address for a MachineTarget.
func machineHost(mt *provisioning.MachineTarget) string {
	if mt.Instance != nil {
		if h := strings.TrimSpace(mt.Instance.PublicIPv4); h != "" {
			return h
		}
	}
	return strings.TrimSpace(mt.Spec.Host)
}

func machineSSHPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

// machineSSHUser delegates to common.MachineSSHUser so the mesh phase
// follows the same SSHUser → AgentUser → root precedence as the rest of
// the apply plan. Install scripts in this package are sudo-prefixed.
func machineSSHUser(m manifestdata.Machine) string {
	return common.MachineSSHUser(m)
}
