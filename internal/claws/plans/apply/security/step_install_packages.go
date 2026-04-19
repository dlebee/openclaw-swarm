package security

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallPackagesStep installs ufw, fail2ban, and unattended-upgrades via apt.
type InstallPackagesStep struct {
	dial SSHDialFunc
}

func NewInstallPackagesStep(opts Options) *InstallPackagesStep {
	return &InstallPackagesStep{dial: opts.SSHDial}
}

func (*InstallPackagesStep) Name() string { return "install-security-packages" }

func (s *InstallPackagesStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := isHostedMachine(t.Payload)
	return ok, nil
}

func (s *InstallPackagesStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
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
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	for _, pkg := range securityPackages {
		installed, err := apt.IsInstalled(client, pkg)
		if err != nil || !installed {
			return false, nil
		}
	}
	return true, nil
}

func (s *InstallPackagesStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := isHostedMachine(t.Payload)
	if !ok {
		return fmt.Errorf("install-security-packages: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(ctx, mt)
	if host == "" {
		return fmt.Errorf("install-security-packages: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSHWithRetry(ctx, s.dial, host, machineSSHPort(mt.Spec), machineBootstrapUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("install-security-packages: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := apt.Update(ctx, client); err != nil {
		return fmt.Errorf("install-security-packages: apt update: %w", err)
	}
	if err := apt.Install(ctx, client, securityPackages...); err != nil {
		return fmt.Errorf("install-security-packages: apt install: %w", err)
	}
	return nil
}

func (s *InstallPackagesStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := isHostedMachine(t.Payload)
	if !ok {
		return fmt.Errorf("install-security-packages verify: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(ctx, mt)
	if host == "" {
		return fmt.Errorf("install-security-packages verify: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineBootstrapUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("install-security-packages verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	for _, pkg := range securityPackages {
		installed, err := apt.IsInstalled(client, pkg)
		if err != nil {
			return fmt.Errorf("install-security-packages verify: %s: %w", pkg, err)
		}
		if !installed {
			return fmt.Errorf("install-security-packages verify: %s not installed", pkg)
		}
	}
	return nil
}

var securityPackages = []string{"ufw", "fail2ban", "unattended-upgrades"}
