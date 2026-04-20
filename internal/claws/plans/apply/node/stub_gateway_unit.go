package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// gatewayStubUnit is the body of a placeholder openclaw-gateway.service
// user unit installed on every node host as a workaround for an upstream
// openclaw bug described in docs/issues/04-node-install-enables-gateway-unit.md.
//
// `openclaw node install` always runs `systemctl --user enable
// openclaw-gateway.service` (see activateSystemdService in openclaw
// src/daemon/systemd.ts:541, which hardcodes resolveGatewaySystemdServiceName
// instead of honouring OPENCLAW_SYSTEMD_UNIT=openclaw-node). On a node-only
// host that file doesn't exist, so the enable fails and the whole install
// transaction unwinds before ~/.openclaw/node.json is persisted.
//
// Writing this no-op unit makes the enable succeed without introducing any
// behaviour: ExecStart=/bin/true runs once if something ever starts the unit
// (nothing in our plan does), RemainAfterExit keeps it reported "active"
// afterwards. Once the upstream fix lands and the pinned openclaw version is
// bumped, this step can be removed.
const gatewayStubUnit = `[Unit]
Description=Placeholder for openclaw-gateway.service (swarm2 issue 04 workaround)

[Service]
Type=oneshot
ExecStart=/bin/true
RemainAfterExit=yes

[Install]
WantedBy=default.target
`

// StubGatewayUnitStep installs the placeholder openclaw-gateway.service
// user unit on a node host before BootstrapNodeStep runs. See
// docs/issues/04-node-install-enables-gateway-unit.md for the upstream bug.
//
// Idempotent: Check returns true once either the stub file exists (we wrote
// it) or the real openclaw-node.service exists (bootstrap has already
// succeeded, so the stub is no longer on the critical path).
type StubGatewayUnitStep struct {
	dial SSHDialFunc
}

func NewStubGatewayUnitStep(opts Options) *StubGatewayUnitStep {
	return &StubGatewayUnitStep{dial: opts.SSHDial}
}

func (*StubGatewayUnitStep) Name() string { return "stub-gateway-unit" }

func (s *StubGatewayUnitStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*NodeTarget)
	return ok, nil
}

func (s *StubGatewayUnitStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", m.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `if [ -f ~/.config/systemd/user/openclaw-gateway.service ] || [ -f ~/.config/systemd/user/openclaw-node.service ]; then
  echo satisfied
else
  echo missing
fi`)
	if err != nil {
		return false, fmt.Errorf("probe gateway/node units on %s: %w", m.Name, err)
	}
	return strings.TrimSpace(out) == "satisfied", nil
}

func (s *StubGatewayUnitStep) Execute(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("stub-gateway-unit: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// XDG_RUNTIME_DIR is not set on non-interactive SSH sessions, but
	// `systemctl --user` refuses to talk to the user manager without it.
	// linger (enabled for the agent user by ensure-agent-user) keeps
	// /run/user/<uid> populated, so exporting it explicitly is safe.
	script := fmt.Sprintf(`set -euo pipefail
export XDG_RUNTIME_DIR=/run/user/$(id -u)
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/openclaw-gateway.service <<'OPENCLAW_GATEWAY_STUB_EOF'
%sOPENCLAW_GATEWAY_STUB_EOF
systemctl --user daemon-reload
`, gatewayStubUnit)

	if out, err := bash.RunOutput(client, script); err != nil {
		return fmt.Errorf("stub-gateway-unit: write placeholder unit: %w\n%s", err, out)
	}
	return nil
}

func (s *StubGatewayUnitStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("stub-gateway-unit verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `test -f ~/.config/systemd/user/openclaw-gateway.service && echo ok || echo missing`)
	if err != nil || strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("stub-gateway-unit verify: placeholder unit not found on disk")
	}
	return nil
}
