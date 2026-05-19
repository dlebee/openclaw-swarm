package security

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/ufw"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

const (
	sshDialRetries    = 10
	sshDialRetryDelay = 3 * time.Second
)

// borrowSSH gets a client from the plan-scoped SSH pool (or dials directly if
// no pool is registered).
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

// borrowSSHWithRetry retries borrowSSH up to sshDialRetries times with backoff.
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
//
// Precedence:
//
//  1. mt.Instance.PublicIPv4 (set in this process by provisioning.create-
//     machine's Check/Execute).
//  2. common.ResolveMachineHost, which consults the plan cache and, on a
//     cold start (`--only security` in a brand-new process), falls through
//     to the plan-scoped HostResolverFn installed by apply.BuildPlan —
//     re-hydrating from the live provider.
//  3. mt.Spec.Host as a final static-SSH fallback.
//
// The ctx-less call site exists on helper paths that never had a ctx
// (test doubles, describe-only tools). Those callers can only rely on
// the Instance / Spec.Host branches.
func machineHost(ctx context.Context, mt *provisioning.MachineTarget) string {
	if mt.Instance != nil {
		if h := strings.TrimSpace(mt.Instance.PublicIPv4); h != "" {
			return h
		}
	}
	if h := common.ResolveMachineHost(ctx, mt.Spec); h != "" {
		return h
	}
	return strings.TrimSpace(mt.Spec.Host)
}

func machineSSHPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

// machineBootstrapUser delegates to common.MachineBootstrapUser. Security
// is the second (and last) phase that dials the machine as the privileged
// bootstrap identity: ufw/fail2ban/unattended-upgrades all need root (or
// sudo) to install and enable. Everything after this phase uses
// common.MachineAgentUser instead — by the time mesh/gateway/node run,
// the agent user has been created by provisioning.EnsureAgentUser and
// services run under it.
func machineBootstrapUser(m manifestdata.Machine) string {
	return common.MachineBootstrapUser(m)
}

const (
	serviceActiveRetries = 8
	serviceActiveDelay   = 2 * time.Second
)

// waitServiceActive polls systemd.IsActive until the unit is running or
// retries are exhausted. Security-phase services are system-level (not user-mode).
func waitServiceActive(ctx context.Context, client *xssh.Client, unit string) error {
	return systemd.WaitActive(ctx, client, unit, false, serviceActiveRetries, serviceActiveDelay)
}

// waitUFWActive polls ufw status until it reports active.
func waitUFWActive(ctx context.Context, client *xssh.Client) error {
	for attempt := 0; attempt < serviceActiveRetries; attempt++ {
		active, err := ufw.IsActive(client)
		if err == nil && active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serviceActiveDelay):
		}
	}
	return fmt.Errorf("ufw is not active after %d checks", serviceActiveRetries)
}

// runScript pipes a bash script to /bin/bash -s on the remote host.
// Used for step-specific logic that doesn't belong in a platformutil package.
func runScript(client *xssh.Client, script string) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run("/bin/bash -s"); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// runScriptOutput pipes a bash script and returns stdout.
func runScriptOutput(client *xssh.Client, script string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run("/bin/bash -s"); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}

// isSecurityApplicable is the shared Applicable check for every security
// step. It returns (target, true) iff the payload describes a machine the
// security phase is allowed to touch:
//
//   - Hosted machines backed by a hosting.Provider (linode, multipass, …)
//     — anything that went through create-machine. Always run.
//   - `type: ssh` machines with apply_security: true — pre-provisioned
//     hosts that the operator has explicitly opted into hardening for.
//
// Bare `type: ssh` entries without the flag are skipped by design: they
// are pre-provisioned and `claws` doesn't own them. The same rule used to
// be hard-coded here as "hosted-only"; the apply_security flag now lets
// operators turn the gate on per-machine for boxes they've decided claws
// can touch (passwordless sudo + image they're happy to see ufw/fail2ban/
// unattended-upgrades land on).
//
// Centralizing this keeps every step's Applicable / Check / Execute /
// Verify aligned and means future machine types are a one-line edit to
// manifestdata.Machine.SecurityPhaseApplies.
func isSecurityApplicable(t interface{}) (*provisioning.MachineTarget, bool) {
	mt, ok := t.(*provisioning.MachineTarget)
	if !ok || mt == nil {
		return nil, false
	}
	if !mt.Spec.SecurityPhaseApplies() {
		return nil, false
	}
	return mt, true
}
