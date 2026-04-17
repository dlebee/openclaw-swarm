package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallNodejsStep checks that Node.js is installed on the target machine.
type InstallNodejsStep struct {
	dial SSHDialFunc
}

func NewInstallNodejsStep(opts Options) *InstallNodejsStep {
	return &InstallNodejsStep{dial: opts.SSHDial}
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
	client, key, err := BorrowSSH(ctx, s.dial, ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `node --version 2>/dev/null || echo missing`)
	if err != nil {
		return false, nil
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
	host, port, user := ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineSSHUser(m)

	script := `set -euo pipefail
if ! command -v node >/dev/null 2>&1; then
  curl -fsSL https://deb.nodesource.com/setup_22.x | sudo bash -
fi
export DEBIAN_FRONTEND=noninteractive
sudo apt-get install -y -qq nodejs
`
	if err := RunBashWithRetry(ctx, s.dial, host, port, user, script); err != nil {
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
	client, key, err := BorrowSSH(ctx, s.dial, ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineSSHUser(m))
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
