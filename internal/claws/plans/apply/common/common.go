// Package common provides shared step implementations reusable across
// multiple apply phases (gateway, node, etc.).
package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
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

// MachineSSHUser returns the user that post-provisioning steps dial the
// machine as. Precedence:
//
//  1. Explicit SSHUser override (rare — power users who want a specific login)
//  2. AgentUser (the default for normal operations; created by
//     provisioning.EnsureAgentUser and granted passwordless sudo)
//  3. "root" (fallback when the manifest opted out of an agent user)
//
// Using the agent user by default is important because it's the only
// non-system account that ensure-agent-user runs `loginctl enable-linger`
// for. Services registered under the agent user's systemd manager therefore
// survive the SSH session disconnecting; root's user manager does not by
// default, which previously killed the openclaw node daemon immediately
// after bootstrap-node finished (breaking pair-node).
//
// Provisioning steps (create-machine, authorize-ssh-key, ensure-agent-user)
// intentionally bypass this helper and always dial as the raw login user
// (root on a fresh Linode) — the agent user doesn't exist yet.
func MachineSSHUser(m manifestdata.Machine) string {
	if u := strings.TrimSpace(m.SSHUser); u != "" {
		return u
	}
	if u := strings.TrimSpace(m.AgentUser); u != "" {
		return u
	}
	return "root"
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

// ---------------------------------------------------------------------------
// Bash-over-SSH helpers with transient-error retry
// ---------------------------------------------------------------------------

// TransientSSHSessionRetries is the maximum number of attempts RunBashWithRetry
// and RunBashOutputWithRetry make against a single host. The first attempt
// uses the pool; subsequent attempts bypass it with a fresh dial so a dead
// pooled connection can't defeat the retry.
const TransientSSHSessionRetries = 3

// RunBashWithRetry executes script via bash over SSH, retrying up to
// TransientSSHSessionRetries times when the session fails with a symptom of
// a dead pooled connection (the x/crypto/ssh "exited without exit status"
// error, EOF, broken pipe, connection reset). Non-transient script errors
// return immediately without retry so real failures aren't masked.
//
// First attempt uses BorrowSSH (pool); retries dial fresh and close the
// client after use. This is the standard pattern for long-running apt/npm
// installs that must be resilient to sshd restarts triggered by
// unattended-upgrades / needrestart on freshly-provisioned machines.
func RunBashWithRetry(ctx context.Context, dial SSHDialFunc, host string, port int, user, script string) error {
	_, err := runBashAttempts(ctx, dial, host, port, user, script, false)
	return err
}

// RunBashOutputWithRetry is the stdout-returning variant of RunBashWithRetry.
func RunBashOutputWithRetry(ctx context.Context, dial SSHDialFunc, host string, port int, user, script string) (string, error) {
	return runBashAttempts(ctx, dial, host, port, user, script, true)
}

func runBashAttempts(ctx context.Context, dial SSHDialFunc, host string, port int, user, script string, wantOutput bool) (string, error) {
	var lastErr error
	for attempt := 0; attempt < TransientSSHSessionRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		var client *xssh.Client
		var key string
		var err error
		if attempt == 0 {
			client, key, err = BorrowSSH(ctx, dial, host, port, user)
		} else {
			client, err = dial(ctx, host, port, user)
		}
		if err != nil {
			lastErr = err
			if !isTransientDialError(err) {
				return "", err
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(SSHDialRetryDelay):
			}
			continue
		}

		var out string
		var runErr error
		if wantOutput {
			out, runErr = bash.RunOutput(client, script)
		} else {
			runErr = bash.Run(client, script)
		}

		if runErr == nil {
			if attempt == 0 {
				ReturnSSH(ctx, key, client)
			} else {
				client.Close()
			}
			return out, nil
		}

		client.Close()
		lastErr = runErr
		if !isTransientSSHSessionErr(runErr) {
			return out, runErr
		}
	}
	return "", fmt.Errorf("bash over ssh %s@%s:%d after %d attempts: %w", user, host, port, TransientSSHSessionRetries, lastErr)
}

func isTransientSSHSessionErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "exited without exit status or exit signal"):
		return true
	case strings.Contains(s, "connection reset by peer"):
		return true
	case strings.Contains(s, "broken pipe"):
		return true
	case strings.Contains(s, "use of closed network connection"):
		return true
	case strings.Contains(s, "unexpected EOF"):
		return true
	}
	return false
}

func isTransientDialError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return true
	case strings.Contains(s, "i/o timeout"):
		return true
	case strings.Contains(s, "no route to host"):
		return true
	case strings.Contains(s, "EOF"):
		return true
	}
	return false
}
