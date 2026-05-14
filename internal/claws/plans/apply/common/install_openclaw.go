package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// OpenclawVersionProvider is optionally implemented by target payloads that
// pin a specific openclaw npm version. When the payload does not implement
// this interface (or returns an empty string), the latest published version
// is installed. The version is only applied during the initial install —
// Check only probes for presence, not the installed version, so subsequent
// applies against an already-installed host are always no-ops regardless of
// what the manifest says.
type OpenclawVersionProvider interface {
	GetOpenclawVersion() string
}

// InstallOpenclawStep checks that openclaw is installed on the target machine.
type InstallOpenclawStep struct {
	dial         SSHDialFunc
	hostResolver HostResolverFn
}

func NewInstallOpenclawStep(opts Options) *InstallOpenclawStep {
	return &InstallOpenclawStep{dial: opts.SSHDial, hostResolver: opts.HostResolver}
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
	host, known := HostKnown(ctx, m, s.hostResolver)
	if !known {
		return false, nil
	}
	client, key, err := BorrowSSH(ctx, s.dial, host, MachineSSHPort(m), MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, OpenclawCLIPreamble()+`openclaw --version 2>/dev/null || echo missing`)
	if err != nil {
		return false, fmt.Errorf("probe openclaw on %s: %w", m.Name, err)
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
	host, port, user := ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineAgentUser(m)

	pkg := "openclaw"
	if vp, ok := t.Payload.(OpenclawVersionProvider); ok {
		if v := strings.TrimSpace(vp.GetOpenclawVersion()); v != "" {
			pkg = "openclaw@" + v
		}
	}
	script := fmt.Sprintf("set -euo pipefail\nsudo npm install -g %s --quiet\n", pkg)
	if err := RunBashWithRetry(ctx, s.dial, host, port, user, script); err != nil {
		return fmt.Errorf("install-openclaw: %w", err)
	}
	return nil
}

func (s *InstallOpenclawStep) Verify(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-openclaw verify: target %q does not provide a machine", t.ID)
	}
	m := mp.GetMachine()
	// `npm install -g openclaw` often triggers a queued needrestart /
	// unattended-upgrades pass that briefly restarts sshd, and Execute's
	// retry already papered over that for the install itself. Verify needs
	// the same resilience or a single post-install SYN timeout fails the
	// phase — matches the rationale on InstallTailscaleStep.Verify.
	client, key, err := BorrowSSHWithRetry(ctx, s.dial, ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-openclaw verify: dial: %w", err)
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, OpenclawCLIPreamble()+`openclaw --version`)
	if err != nil {
		return fmt.Errorf("install-openclaw verify: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("install-openclaw verify: empty version output")
	}
	return nil
}
