package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnsureNodejsStep checks that Node.js is installed on the gateway machine.
type EnsureNodejsStep struct {
	dial SSHDialFunc
}

func NewEnsureNodejsStep(opts Options) *EnsureNodejsStep {
	return &EnsureNodejsStep{dial: opts.SSHDial}
}

func (*EnsureNodejsStep) Name() string { return "install-nodejs" }

func (*EnsureNodejsStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*GatewayTarget)
	return ok, nil
}

func (s *EnsureNodejsStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	gt, ok := t.Payload.(*GatewayTarget)
	if !ok || gt == nil {
		return false, nil
	}
	if s.dial == nil {
		return false, nil
	}
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `set -euo pipefail
node --version 2>/dev/null || echo missing
`)
	if err != nil {
		return false, nil
	}
	return !strings.Contains(strings.TrimSpace(out), "missing"), nil
}

func (s *EnsureNodejsStep) Execute(ctx context.Context, t scaffold.Target) error {
	gt, ok := t.Payload.(*GatewayTarget)
	if !ok || gt == nil {
		return fmt.Errorf("install-nodejs: expected *GatewayTarget for %q", t.ID)
	}
	if s.dial == nil {
		return fmt.Errorf("install-nodejs: SSH dialer not configured")
	}
	m := gt.Machine
	client, key, err := borrowSSHWithRetry(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-nodejs: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := bash.Run(client, `set -euo pipefail
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo bash -
`); err != nil {
		return fmt.Errorf("install-nodejs: add nodesource repo: %w", err)
	}
	return apt.Install(client, "nodejs")
}

func (s *EnsureNodejsStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt, ok := t.Payload.(*GatewayTarget)
	if !ok || gt == nil {
		return fmt.Errorf("install-nodejs verify: expected *GatewayTarget for %q", t.ID)
	}
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-nodejs verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `set -euo pipefail
node --version
`)
	if err != nil {
		return fmt.Errorf("install-nodejs verify: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "v") {
		return fmt.Errorf("install-nodejs verify: unexpected node version output: %q", out)
	}
	return nil
}
