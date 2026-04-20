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
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		// Machine not yet provisioned: we don't know whether the
		// systemd unit exists, but the bootstrap WILL apply once
		// provisioning creates the VM. Returning true here surfaces
		// "will execute" in the probe UI instead of "applicable:
		// dial :22: connection refused".
		return true, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `test -f ~/.config/systemd/user/openclaw-node.service && echo exists || echo missing`)
	if err != nil {
		return false, fmt.Errorf("probe node unit on %s: %w", m.Name, err)
	}
	if strings.TrimSpace(out) == "missing" {
		return true, nil
	}
	return false, nil // unit exists → already installed
}

func (s *BootstrapNodeStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `test -f ~/.config/systemd/user/openclaw-node.service && echo exists || echo missing`)
	if err != nil {
		return false, fmt.Errorf("probe node unit on %s: %w", m.Name, err)
	}
	return strings.TrimSpace(out) != "missing", nil
}

func (s *BootstrapNodeStep) Execute(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("bootstrap-node: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// Read the gateway token from the gateway's config.
	gwMach := nt.GWMach
	gwClient, gwKey, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, gwMach), common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
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

	gwHost := nt.GatewayInternalHost(ctx, s.dial)
	if strings.TrimSpace(gwHost) == "" {
		return fmt.Errorf("bootstrap-node: gateway host not resolved for %q (provisioning phase must run first)", nt.GWMach.Name)
	}

	var envPrefix string
	if gwService.NeedsInsecureWS(nt.Gateway) {
		envPrefix = "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 "
	}

	// `openclaw node install` invokes `systemctl --user enable` internally,
	// which requires XDG_RUNTIME_DIR on non-interactive SSH sessions.
	// linger (enabled by ensure-agent-user) keeps /run/user/<uid> alive.
	script := fmt.Sprintf(`set -euo pipefail
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export OPENCLAW_GATEWAY_TOKEN=%q
%sopenclaw node install --host %q --port 18789 --display-name %q --runtime node --force
`, token, envPrefix, gwHost, nt.Spec.Name)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("bootstrap-node: openclaw node install failed: %w\n%s", err, out)
	}

	// The unit now exists. Write the env drop-in and restart so the service
	// picks up OPENCLAW_ALLOW_INSECURE_PRIVATE_WS (not captured by the
	// upstream install — see docs/issues/01-insecure-private-ws-not-bootstrapable.md)
	// plus the startup-optimisation env (NODE_COMPILE_CACHE, OPENCLAW_NO_RESPAWN).
	//
	// The cache directory must exist before the restart below or Node will
	// silently disable caching for this session. configure-node would normally
	// mkdir it later, but its Check() sees no drift after bootstrap already
	// wrote the env and skips Execute — so we have to create it here too.
	if err := gwService.EnsureNodeCompileCacheDir(client); err != nil {
		return fmt.Errorf("bootstrap-node: %w", err)
	}
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
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("bootstrap-node verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// Verify the systemd unit was created and references the correct gateway host.
	out, err := bash.RunOutput(client, `cat ~/.config/systemd/user/openclaw-node.service 2>/dev/null || echo "(not found)"`)
	if err != nil || strings.Contains(out, "(not found)") {
		return fmt.Errorf("bootstrap-node verify: openclaw-node.service not found")
	}
	gwHost := nt.GatewayInternalHost(ctx, s.dial)
	if !strings.Contains(out, gwHost) {
		return fmt.Errorf("bootstrap-node verify: unit does not reference gateway host %q", gwHost)
	}
	return nil
}
