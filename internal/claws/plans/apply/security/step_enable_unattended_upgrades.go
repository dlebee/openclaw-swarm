package security

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemctl"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnableUnattendedUpgradesStep enables unattended-upgrades via systemctl.
// Check verifies the package is installed and the service is enabled.
type EnableUnattendedUpgradesStep struct {
	dial SSHDialFunc
}

func NewEnableUnattendedUpgradesStep(opts Options) *EnableUnattendedUpgradesStep {
	return &EnableUnattendedUpgradesStep{dial: opts.SSHDial}
}

func (*EnableUnattendedUpgradesStep) Name() string { return "enable-unattended-upgrades" }

func (s *EnableUnattendedUpgradesStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := isLinodeMachine(t.Payload)
	return ok, nil
}

func (s *EnableUnattendedUpgradesStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := isLinodeMachine(t.Payload)
	if !ok {
		return false, nil
	}
	host := machineHost(mt)
	if host == "" {
		return false, nil
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineSSHUser(mt.Spec))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	installed, err := apt.IsInstalled(client, "unattended-upgrades")
	if err != nil || !installed {
		return false, nil
	}
	return true, nil
}

func (s *EnableUnattendedUpgradesStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := isLinodeMachine(t.Payload)
	if !ok {
		return fmt.Errorf("enable-unattended-upgrades: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(mt)
	if host == "" {
		return fmt.Errorf("enable-unattended-upgrades: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSHWithRetry(ctx, s.dial, host, machineSSHPort(mt.Spec), machineSSHUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("enable-unattended-upgrades: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := systemctl.Enable(client, "unattended-upgrades"); err != nil {
		return fmt.Errorf("enable-unattended-upgrades: %w", err)
	}
	return nil
}

func (s *EnableUnattendedUpgradesStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := isLinodeMachine(t.Payload)
	if !ok {
		return fmt.Errorf("enable-unattended-upgrades verify: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(mt)
	if host == "" {
		return fmt.Errorf("enable-unattended-upgrades verify: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineSSHUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("enable-unattended-upgrades verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	installed, err := apt.IsInstalled(client, "unattended-upgrades")
	if err != nil {
		return fmt.Errorf("enable-unattended-upgrades verify: %w", err)
	}
	if !installed {
		return fmt.Errorf("enable-unattended-upgrades verify: package not installed")
	}
	return nil
}
