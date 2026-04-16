package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// BootstrapNodeStep runs `openclaw node install` for first-time node setup.
// Not applicable when the systemd unit already exists (node already installed).
type BootstrapNodeStep struct {
	dial SSHDialFunc
}

func NewBootstrapNodeStep(opts Options) *BootstrapNodeStep {
	return &BootstrapNodeStep{dial: opts.SSHDial}
}

func (*BootstrapNodeStep) Name() string { return "bootstrap-node" }

func (s *BootstrapNodeStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	nt, ok := t.Payload.(*NodeTarget)
	if !ok || nt == nil {
		return false, nil
	}
	if s.dial == nil {
		return false, nil
	}
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return true, nil // host unreachable → assume not yet bootstrapped
	}
	defer common.ReturnSSH(ctx, key, client)

	// Check if the systemd unit exists (written by `openclaw node install`).
	out, err := bash.RunOutput(client, `test -f ~/.config/systemd/user/openclaw-node.service && echo exists || echo missing`)
	if err != nil || strings.TrimSpace(out) == "missing" {
		return true, nil
	}
	return false, nil // unit exists → already installed
}

func (s *BootstrapNodeStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `test -f ~/.config/systemd/user/openclaw-node.service && echo exists || echo missing`)
	if err != nil || strings.TrimSpace(out) == "missing" {
		return false, nil
	}
	return true, nil
}

func (s *BootstrapNodeStep) Execute(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("bootstrap-node: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// Read the gateway token from the gateway's config.
	gwMach := nt.GWMach
	gwClient, gwKey, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(gwMach), common.MachineSSHPort(gwMach), common.MachineSSHUser(gwMach))
	if err != nil {
		return fmt.Errorf("bootstrap-node: dial gateway for token: %w", err)
	}
	gwHome, err := gwService.ResolveHome(gwClient)
	if err != nil {
		common.ReturnSSH(ctx, gwKey, gwClient)
		return fmt.Errorf("bootstrap-node: resolve gateway home: %w", err)
	}
	token, err := gwService.ReadToken(gwClient, gwHome)
	common.ReturnSSH(ctx, gwKey, gwClient)
	if err != nil {
		return fmt.Errorf("bootstrap-node: read gateway token: %w", err)
	}
	if token == "" {
		return fmt.Errorf("bootstrap-node: gateway token is empty (gateway not bootstrapped?)")
	}

	gwHost := nt.GatewayInternalHost()

	var envPrefix string
	if gwService.NeedsInsecureWS(nt.Gateway) {
		envPrefix = "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 "
	}

	script := fmt.Sprintf(`set -euo pipefail
export OPENCLAW_GATEWAY_TOKEN=%q
%sopenclaw node install --host %q --port 18789 --display-name %q --runtime node --force
`, token, envPrefix, gwHost, nt.Spec.Name)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("bootstrap-node: openclaw node install failed: %w\n%s", err, out)
	}

	// The unit now exists. Write the env drop-in and restart so the service
	// picks up OPENCLAW_ALLOW_INSECURE_PRIVATE_WS (not captured by the
	// upstream install — see docs/issues/01-insecure-private-ws-not-bootstrapable.md).
	desiredEnv := nodeEnv(nt)
	if len(desiredEnv) > 0 {
		if err := systemd.WriteEnvDropIn(client, nodeUnit, true, desiredEnv); err != nil {
			return fmt.Errorf("bootstrap-node: write env drop-in: %w", err)
		}
		if err := systemd.Restart(client, nodeUnit, true); err != nil {
			return fmt.Errorf("bootstrap-node: restart after env drop-in: %w", err)
		}
	}

	return nil
}

func (s *BootstrapNodeStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("bootstrap-node verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// Verify the systemd unit was created and references the correct gateway host.
	out, err := bash.RunOutput(client, `cat ~/.config/systemd/user/openclaw-node.service 2>/dev/null || echo "(not found)"`)
	if err != nil || strings.Contains(out, "(not found)") {
		return fmt.Errorf("bootstrap-node verify: openclaw-node.service not found")
	}
	if !strings.Contains(out, nt.GatewayInternalHost()) {
		return fmt.Errorf("bootstrap-node verify: unit does not reference gateway host %q", nt.GatewayInternalHost())
	}
	return nil
}
