package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallOpenclawStep checks that openclaw is installed on the target machine.
type InstallOpenclawStep struct {
	dial SSHDialFunc
}

func NewInstallOpenclawStep(opts Options) *InstallOpenclawStep {
	return &InstallOpenclawStep{dial: opts.SSHDial}
}

func (*InstallOpenclawStep) Name() string { return "install-openclaw" }

func (*InstallOpenclawStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(MachineProvider)
	return ok, nil
}

func (s *InstallOpenclawStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return false, nil
	}
	if s.dial == nil {
		return false, nil
	}
	m := mp.GetMachine()
	client, key, err := BorrowSSH(ctx, s.dial, MachineHost(m), MachineSSHPort(m), MachineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `openclaw --version 2>/dev/null || echo missing`)
	if err != nil {
		return false, nil
	}
	return !strings.Contains(strings.TrimSpace(out), "missing"), nil
}

func (s *InstallOpenclawStep) Execute(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-openclaw: target %q does not provide a machine", t.ID)
	}
	if s.dial == nil {
		return fmt.Errorf("install-openclaw: SSH dialer not configured")
	}
	m := mp.GetMachine()
	client, key, err := BorrowSSHWithRetry(ctx, s.dial, MachineHost(m), MachineSSHPort(m), MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-openclaw: %w", err)
	}
	defer ReturnSSH(ctx, key, client)

	script := `set -euo pipefail
sudo npm install -g openclaw --quiet
`
	return bash.Run(client, script)
}

func (s *InstallOpenclawStep) Verify(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-openclaw verify: target %q does not provide a machine", t.ID)
	}
	m := mp.GetMachine()
	client, key, err := BorrowSSH(ctx, s.dial, MachineHost(m), MachineSSHPort(m), MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-openclaw verify: dial: %w", err)
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `openclaw --version`)
	if err != nil {
		return fmt.Errorf("install-openclaw verify: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("install-openclaw verify: empty version output")
	}
	return nil
}
