package security

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/ufw"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnableUFWStep configures and enables UFW, allowing the machine's SSH port.
type EnableUFWStep struct {
	dial SSHDialFunc
}

func NewEnableUFWStep(opts Options) *EnableUFWStep {
	return &EnableUFWStep{dial: opts.SSHDial}
}

func (*EnableUFWStep) Name() string { return "enable-ufw" }

func (s *EnableUFWStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := isHostedMachine(t.Payload)
	return ok, nil
}

func (s *EnableUFWStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := isHostedMachine(t.Payload)
	if !ok {
		return false, nil
	}
	host := machineHost(ctx, mt)
	if host == "" {
		return false, nil
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineBootstrapUser(mt.Spec))
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", host, err)
	}
	defer returnSSH(ctx, key, client)

	active, err := ufw.IsActive(client)
	if err != nil {
		return false, fmt.Errorf("probe ufw on %s: %w", host, err)
	}
	return active, nil
}

func (s *EnableUFWStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := isHostedMachine(t.Payload)
	if !ok {
		return fmt.Errorf("enable-ufw: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(ctx, mt)
	if host == "" {
		return fmt.Errorf("enable-ufw: no reachable host for %q", t.ID)
	}
	port := machineSSHPort(mt.Spec)
	client, key, err := borrowSSHWithRetry(ctx, s.dial, host, port, machineBootstrapUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("enable-ufw: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := ufw.AllowPort(client, port, "tcp"); err != nil {
		return fmt.Errorf("enable-ufw: allow ssh port %d: %w", port, err)
	}
	if err := ufw.Enable(client); err != nil {
		return fmt.Errorf("enable-ufw: enable: %w", err)
	}
	return nil
}

func (s *EnableUFWStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := isHostedMachine(t.Payload)
	if !ok {
		return fmt.Errorf("enable-ufw verify: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(ctx, mt)
	if host == "" {
		return fmt.Errorf("enable-ufw verify: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineBootstrapUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("enable-ufw verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := waitUFWActive(ctx, client); err != nil {
		return fmt.Errorf("enable-ufw verify: %w", err)
	}
	return nil
}
