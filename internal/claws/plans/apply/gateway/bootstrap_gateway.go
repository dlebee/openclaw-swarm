package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// BootstrapGatewayStep runs `openclaw onboard --non-interactive --install-daemon`
// for first-time gateway setup. Not applicable when the gateway is already
// onboarded (config + token side-file exist).
type BootstrapGatewayStep struct {
	dial SSHDialFunc
}

func NewBootstrapGatewayStep(opts Options) *BootstrapGatewayStep {
	return &BootstrapGatewayStep{dial: opts.SSHDial}
}

func (*BootstrapGatewayStep) Name() string { return "bootstrap-gateway" }

// Applicable returns true only when the gateway has not been onboarded yet
// (no config file on the remote host).
func (s *BootstrapGatewayStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
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

	home, err := ResolveHome(client)
	if err != nil {
		return false, fmt.Errorf("bootstrap-gateway: %w", err)
	}
	exists, err := ConfigExists(client, home)
	if err != nil {
		return false, fmt.Errorf("bootstrap-gateway: %w", err)
	}
	return !exists, nil
}

// Check returns true when the gateway token side-file already exists and the
// gateway service is active. In practice this should rarely return true because
// Applicable already filters out onboarded gateways.
func (s *BootstrapGatewayStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	home, err := ResolveHome(client)
	if err != nil {
		return false, nil
	}
	tok, err := ReadToken(client, home)
	if err != nil || tok == "" {
		return false, nil
	}
	return true, nil
}

// Execute runs the non-interactive onboarding command with --install-daemon.
func (s *BootstrapGatewayStep) Execute(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSHWithRetry(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("bootstrap-gateway: %w", err)
	}
	defer returnSSH(ctx, key, client)

	token, err := GenerateToken()
	if err != nil {
		return fmt.Errorf("bootstrap-gateway: %w", err)
	}

	bind := DesiredBind(gt.Spec)

	var envPrefix string
	if NeedsInsecureWS(gt.Spec) {
		envPrefix = "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 "
	}

	script := fmt.Sprintf(`set -euo pipefail
%sopenclaw onboard --non-interactive --mode local --auth-choice skip \
  --gateway-bind %s --gateway-token %q \
  --install-daemon --skip-health --accept-risk --json
`, envPrefix, bind, token)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("bootstrap-gateway: onboard failed: %w\n%s", err, out)
	}

	home, err := ResolveHome(client)
	if err != nil {
		return fmt.Errorf("bootstrap-gateway: %w", err)
	}

	// Write the token side-file since `openclaw config get` redacts secrets.
	sideFileScript := fmt.Sprintf(`set -euo pipefail
mkdir -p %s/.openclaw
printf '%%s' %q > %s/%s
chmod 600 %s/%s
`, home, token, home, tokenSideFile, home, tokenSideFile)

	if err := bash.Run(client, sideFileScript); err != nil {
		return fmt.Errorf("bootstrap-gateway: write token side-file: %w", err)
	}

	return nil
}

// Verify confirms the gateway is listening on port 18789.
func (s *BootstrapGatewayStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("bootstrap-gateway verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := HealthCheck(client, healthRetries, healthDelay); err != nil {
		// Grab logs for diagnostics.
		logs, _ := bash.RunOutput(client, strings.Join([]string{
			`export XDG_RUNTIME_DIR=/run/user/$(id -u)`,
			`journalctl --user -u openclaw-gateway -n 20 --no-pager 2>/dev/null || true`,
		}, "\n"))
		return fmt.Errorf("bootstrap-gateway verify: %w\nrecent logs:\n%s", err, logs)
	}
	return nil
}
