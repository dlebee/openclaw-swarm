// Package common provides shared step implementations reusable across
// multiple apply phases (gateway, node, etc.).
package common

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc func(ctx context.Context, host string, port int, user string) (*xssh.Client, error)

// MachineProvider is implemented by any target payload that references a
// manifest machine (GatewayTarget, NodeTarget, etc.). Shared steps like
// install-nodejs and install-openclaw use this to avoid coupling to a
// specific target type.
type MachineProvider interface {
	GetMachine() manifestdata.Machine
}

// Options configures shared steps.
type Options struct {
	SSHDial SSHDialFunc
}

// ---------------------------------------------------------------------------
// SSH helpers (shared across phases)
// ---------------------------------------------------------------------------

const (
	SSHDialRetries    = 10
	SSHDialRetryDelay = 3 * time.Second
)

func MachineHost(m manifestdata.Machine) string {
	return strings.TrimSpace(m.Host)
}

// ResolveMachineHost returns the reachable SSH address for m, preferring the
// plan-cache entry recorded by create-machine (for Linode instances, both
// existing-discovered in Check and fresh-created in Execute) over the
// manifest's static Spec.Host. Falls back to Spec.Host when no cache entry
// exists — which is exactly how non-Linode (static SSH) machines work today.
//
// This lets every downstream phase — mesh, gateway, channels, node, agents,
// automations — dial Linode machines by their freshly resolved IP without
// each phase having to thread a *provisioning.MachineTarget pointer through
// its own target struct.
func ResolveMachineHost(ctx context.Context, m manifestdata.Machine) string {
	if h, ok := scaffold.LookupPlanMachineHost(ctx, m.Name); ok && h != "" {
		return h
	}
	return MachineHost(m)
}

func MachineSSHPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

func MachineSSHUser(m manifestdata.Machine) string {
	u := strings.TrimSpace(m.SSHUser)
	if u == "" {
		return "root"
	}
	return u
}

func BorrowSSH(ctx context.Context, dial SSHDialFunc, host string, port int, user string) (*xssh.Client, string, error) {
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

func BorrowSSHWithRetry(ctx context.Context, dial SSHDialFunc, host string, port int, user string) (*xssh.Client, string, error) {
	var lastErr error
	for attempt := 0; attempt < SSHDialRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		c, key, err := BorrowSSH(ctx, dial, host, port, user)
		if err == nil {
			return c, key, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(SSHDialRetryDelay):
		}
	}
	return nil, "", fmt.Errorf("dial %s@%s:%d after %d retries: %w", user, host, port, SSHDialRetries, lastErr)
}

func ReturnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := sshPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}

func sshPool(ctx context.Context) *clawssh.Pool {
	return provisioning.SSHPool(ctx)
}
