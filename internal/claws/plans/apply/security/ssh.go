package security

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemctl"
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
// Linode machines use Instance.PublicIPv4; static SSH machines use Spec.Host.
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

func machineSSHUser(m manifestdata.Machine) string {
	u := strings.TrimSpace(m.SSHUser)
	if u == "" {
		return "root"
	}
	return u
}

const (
	serviceActiveRetries = 8
	serviceActiveDelay   = 2 * time.Second
)

// waitServiceActive polls systemctl is-active until the unit is running or
// retries are exhausted. Handles the race where `enable --now` returns before
// the service is fully up.
func waitServiceActive(ctx context.Context, client *xssh.Client, unit string) error {
	for attempt := 0; attempt < serviceActiveRetries; attempt++ {
		active, err := systemctl.IsActive(client, unit)
		if err == nil && active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serviceActiveDelay):
		}
	}
	return fmt.Errorf("%s is not active after %d checks", unit, serviceActiveRetries)
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

// isLinodeMachine is the shared Applicable check for all security steps.
func isLinodeMachine(t interface{}) (*provisioning.MachineTarget, bool) {
	mt, ok := t.(*provisioning.MachineTarget)
	if !ok || mt == nil {
		return nil, false
	}
	if mt.Spec.Type != manifestdata.MachineTypeLinode {
		return nil, false
	}
	return mt, true
}
