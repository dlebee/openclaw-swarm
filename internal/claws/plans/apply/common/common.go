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

// HostResolverFn is a last-resort lookup installed on the plan cache by
// apply.BuildPlan. ResolveMachineHost invokes it when both the plan cache
// (populated by provisioning.create-machine) and the manifest's Spec.Host
// are empty — the cold-start case where an operator runs
// `claws apply --only <phase>` without provisioning having written any
// per-machine host entry in this process.
//
// The resolver is expected to query the hosting provider (ListByTag /
// match-by-label) and record the discovered PublicIPv4 into the plan cache
// via scaffold.RecordPlanMachineHost before returning, so subsequent calls
// in the same run are zero-cost cache hits.
//
// Returning ("", false, nil) means "no fallback available" (e.g. SSH-only
// machine, empty manifest prefix); returning an error aborts the calling
// step with that error.
type HostResolverFn func(ctx context.Context, machineName string) (string, bool, error)

const planCacheHostResolverKey = "APPLY_HOST_RESOLVER"

// RegisterHostResolver installs fn on ctx's plan cache so every subsequent
// ResolveMachineHost call in the same run can trigger lazy host re-hydration
// when the per-machine cache entry is still absent. Safe to call multiple
// times; the most recent fn wins.
func RegisterHostResolver(ctx context.Context, fn HostResolverFn) {
	if fn == nil {
		return
	}
	scaffold.PlanCacheSet(ctx, planCacheHostResolverKey, fn)
}

// ResolveMachineHost returns the reachable SSH address for m, preferring the
// plan-cache entry recorded by create-machine (for Linode instances, both
// existing-discovered in Check and fresh-created in Execute) over the
// manifest's static Spec.Host. Falls back to Spec.Host when no cache entry
// exists — which is exactly how non-Linode (static SSH) machines work today.
//
// On a cold start (e.g. `claws apply --only security` in a brand-new process
// where provisioning never ran this invocation), both the cache and the
// manifest Host are empty for hosted machines. Before surrendering we consult
// the plan-cache-installed HostResolverFn (see RegisterHostResolver), which
// apply.BuildPlan wires up to call provisioning.ResolveHostedInstances
// against the live provider — reconstructing PublicIPv4 from the running
// infrastructure itself rather than from a previous phase's in-memory state.
//
// Errors from the resolver are swallowed (returns empty string) so callers
// can still distinguish "no host" from "resolver failed" via the downstream
// dial failure; this mirrors how LookupPlanMachineHost's ok=false used to
// fall through to MachineHost silently.
func ResolveMachineHost(ctx context.Context, m manifestdata.Machine) string {
	if h, ok := scaffold.LookupPlanMachineHost(ctx, m.Name); ok && h != "" {
		return h
	}
	if h := MachineHost(m); h != "" {
		return h
	}
	if v, ok := scaffold.PlanCacheGet(ctx, planCacheHostResolverKey); ok {
		if fn, ok := v.(HostResolverFn); ok && fn != nil {
			if h, ok, err := fn(ctx, m.Name); err == nil && ok && strings.TrimSpace(h) != "" {
				return strings.TrimSpace(h)
			}
		}
	}
	return ""
}

// HostKnown returns the reachable host for m and whether it is non-empty.
//
// A step's Check / Applicable must short-circuit when !ok — otherwise a
// Borrow/Dial against an empty host produces the confusing
//
//	dial tcp :22: connect: connection refused
//
// which the probe UI then surfaces as "check: dial ..." for every phase
// that runs AFTER the provisioning phase in a cold-start `claws apply`:
// provisioning hasn't Execute'd yet at probe time, so the machine does
// not exist in the hosting provider and ResolveMachineHost correctly
// returns "". The honest answer in that case is "will execute once the
// machine is provisioned", not "check failed".
//
// Convention for the probe UI:
//
//	Check:      host, ok := common.HostKnown(ctx, m); if !ok { return false, nil }
//	Applicable: host, ok := common.HostKnown(ctx, m); if !ok { return true,  nil }
//
// Check returns (false, nil) — "not yet satisfied, please execute";
// Applicable returns (true,  nil) — "this step WILL run once the machine
// exists, don't mark it not-applicable". Execute paths should never see
// an empty host (provisioning always runs before the caller in a real
// plan); if they do, it's a programmer error and the caller should
// surface the ResolveMachineHost="" explicitly.
func HostKnown(ctx context.Context, m manifestdata.Machine) (string, bool) {
	host := ResolveMachineHost(ctx, m)
	return host, host != ""
}

func MachineSSHPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

// MachineBootstrapUser returns the privileged identity that the
// provisioning and security phases dial the machine as. This is the
// "escalated root" account used only to bring the box up and lock it
// down — never for app-level work (installing openclaw, configuring
// gateways, running agents, etc.). Those phases use MachineAgentUser
// instead.
//
// Precedence:
//
//  1. Explicit BootstrapUser from the manifest
//  2. "root" (fresh Linode image default)
//
// Deliberately does NOT fall back to AgentUser: the two users are
// architecturally distinct roles (bootstrap vs. ongoing agent). If a
// manifest omits both, it's opting into root bootstrap.
//
// Only provisioning/* and security/* may call this. Any other caller is
// a design bug — ops-time phases must identify as the agent, not as the
// bootstrap user (which may be disabled or restricted after hardening).
func MachineBootstrapUser(m manifestdata.Machine) string {
	if u := strings.TrimSpace(m.BootstrapUser); u != "" {
		return u
	}
	return "root"
}

// MachineAgentUser returns the unprivileged identity that every
// post-security phase (mesh, node, gateway, agents, channels,
// automations, install_openclaw, install_nodejs) dials the machine as.
// This is the account created by provisioning.EnsureAgentUser and
// granted passwordless sudo; services registered under this user's
// systemd manager also benefit from `loginctl enable-linger` so they
// survive SSH disconnects.
//
// Precedence:
//
//  1. Explicit AgentUser from the manifest
//  2. "root" (fallback when the manifest opted out of an agent user,
//     e.g. static ssh-type machines managed by an existing operator)
func MachineAgentUser(m manifestdata.Machine) string {
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
