package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnsureOpenclawStep checks that openclaw is installed on the gateway machine,
// and installs or updates it to the version specified in the gateway spec.
type EnsureOpenclawStep struct {
	dial SSHDialFunc
}

func NewEnsureOpenclawStep(opts Options) *EnsureOpenclawStep {
	return &EnsureOpenclawStep{dial: opts.SSHDial}
}

func (*EnsureOpenclawStep) Name() string { return "install-openclaw" }

func (*EnsureOpenclawStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*GatewayTarget)
	return ok, nil
}

func (s *EnsureOpenclawStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
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
openclaw --version 2>/dev/null || echo missing
`)
	if err != nil {
		return false, nil
	}
	return !strings.Contains(strings.TrimSpace(out), "missing"), nil
}

func (s *EnsureOpenclawStep) Execute(ctx context.Context, t scaffold.Target) error {
	gt, ok := t.Payload.(*GatewayTarget)
	if !ok || gt == nil {
		return fmt.Errorf("install-openclaw: expected *GatewayTarget for %q", t.ID)
	}
	if s.dial == nil {
		return fmt.Errorf("install-openclaw: SSH dialer not configured")
	}
	m := gt.Machine
	client, key, err := borrowSSHWithRetry(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-openclaw: %w", err)
	}
	defer returnSSH(ctx, key, client)

	version := strings.TrimSpace(gt.Spec.OpenclawVersion)
	pkg := "openclaw"
	if version != "" && version != "latest" {
		pkg = fmt.Sprintf("openclaw@%s", version)
	}
	script := fmt.Sprintf(`set -euo pipefail
sudo npm install -g %s --quiet
`, pkg)
	return bash.Run(client, script)
}

func (s *EnsureOpenclawStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt, ok := t.Payload.(*GatewayTarget)
	if !ok || gt == nil {
		return fmt.Errorf("install-openclaw verify: expected *GatewayTarget for %q", t.ID)
	}
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-openclaw verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `set -euo pipefail
openclaw --version
`)
	if err != nil {
		return fmt.Errorf("install-openclaw verify: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("install-openclaw verify: empty version output")
	}
	return nil
}
