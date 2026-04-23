package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallNodejsStep checks that Node.js is installed on the target machine.
type InstallNodejsStep struct {
	dial         SSHDialFunc
	hostResolver HostResolverFn
}

func NewInstallNodejsStep(opts Options) *InstallNodejsStep {
	return &InstallNodejsStep{dial: opts.SSHDial, hostResolver: opts.HostResolver}
}

func (*InstallNodejsStep) Name() string { return "install-nodejs" }

func (*InstallNodejsStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(MachineProvider)
	return ok, nil
}

func (s *InstallNodejsStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return false, nil
	}
	if s.dial == nil {
		return false, nil
	}
	m := mp.GetMachine()
	host, known := HostKnown(ctx, m, s.hostResolver)
	if !known {
		return false, nil
	}
	client, key, err := BorrowSSH(ctx, s.dial, host, MachineSSHPort(m), MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `node --version 2>/dev/null || echo missing`)
	if err != nil {
		return false, fmt.Errorf("probe node on %s: %w", m.Name, err)
	}
	return !strings.Contains(strings.TrimSpace(out), "missing"), nil
}

func (s *InstallNodejsStep) Execute(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-nodejs: target %q does not provide a machine", t.ID)
	}
	if s.dial == nil {
		return fmt.Errorf("install-nodejs: SSH dialer not configured")
	}
	m := mp.GetMachine()
	host, port, user := ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineAgentUser(m)

	script := `set -euo pipefail
if ! command -v node >/dev/null 2>&1; then
  curl -fsSL --http1.1 --retry 5 --retry-all-errors --retry-delay 3 https://deb.nodesource.com/setup_22.x | sudo bash -
fi
export DEBIAN_FRONTEND=noninteractive
sudo apt-get install -y -qq nodejs
`
	// apt.WithLockRetry retries the whole script on apt/dpkg lock
	// contention (apt-daily, unattended-upgrades). RunBashWithRetry
	// already handles transient SSH session drops; the two layers
	// target different failure classes.
	if err := apt.WithLockRetry(ctx, apt.RetryOpts{}, func() error {
		return RunBashWithRetry(ctx, s.dial, host, port, user, script)
	}); err != nil {
		return fmt.Errorf("install-nodejs: %w", err)
	}
	return nil
}

func (s *InstallNodejsStep) Verify(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-nodejs verify: target %q does not provide a machine", t.ID)
	}
	m := mp.GetMachine()
	// The nodesource setup script + apt install often trip needrestart,
	// which restarts sshd mid-session. Retry the dial so a single SYN
	// timeout doesn't fail the phase — same precedent as
	// InstallTailscaleStep.Verify.
	client, key, err := BorrowSSHWithRetry(ctx, s.dial, ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-nodejs verify: dial: %w", err)
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `node --version`)
	if err != nil {
		return fmt.Errorf("install-nodejs verify: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "v") {
		return fmt.Errorf("install-nodejs verify: unexpected node version output: %q", out)
	}
	return nil
}
