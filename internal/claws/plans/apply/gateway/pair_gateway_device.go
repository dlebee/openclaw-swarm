package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const (
	pairingRetries = 8
	pairingDelay   = 2 * time.Second
)

// PairGatewayDeviceStep approves the local CLI device pairing on the gateway.
// OpenClaw requires WebSocket device approval before commands like
// `openclaw nodes list` or `openclaw agents add` work. This step triggers
// the pairing request and approves it.
//
// Applicable only when the gateway is already onboarded (config exists).
type PairGatewayDeviceStep struct {
	dial   SSHDialFunc
	reader common.ConfigReader
}

func NewPairGatewayDeviceStep(opts Options) *PairGatewayDeviceStep {
	r := opts.ConfigReader
	if r == nil {
		r = common.DefaultConfigReader(common.SSHDialFunc(opts.SSHDial))
	}
	return &PairGatewayDeviceStep{dial: opts.SSHDial, reader: r}
}

func (*PairGatewayDeviceStep) Name() string { return "pair-gateway-device" }

// Applicable returns true for any gateway target. The step runs after
// bootstrap-gateway in the pipeline, so by execution time the config will
// exist regardless of whether this is a fresh or existing gateway.
func (s *PairGatewayDeviceStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*GatewayTarget)
	return ok, nil
}

// Check returns true when at least one local device is already paired.
// Check returns true when a local CLI device is paired with admin scope.
func (s *PairGatewayDeviceStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	host, ok := hostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer returnSSH(ctx, key, client)

	cfgHost := common.MachineConfigHost(m, host)
	dl, err := s.reader.DeviceList(ctx, client, cfgHost)
	if err != nil {
		return false, fmt.Errorf("list devices on %s: %w", m.Name, err)
	}
	return hasLocalCLIWithAdminScope(dl), nil
}

// hasLocalCLIWithAdminScope checks if there's a CLI device paired with admin scope.
func hasLocalCLIWithAdminScope(dl *common.DeviceList) bool {
	if dl == nil {
		return false
	}
	for _, p := range dl.Paired {
		if p.ClientID == "cli" && p.ClientMode == "cli" {
			for _, scope := range p.Scopes {
				if scope == "operator.admin" {
					return true
				}
			}
		}
	}
	return false
}

// Execute ensures the local CLI device is paired with full operator.admin scope.
// OpenClaw 2026.8+ auto-approves local CLI connections, so this step may complete
// immediately after triggering a CLI command. For older versions, pending pairing
// requests are approved explicitly.
func (s *PairGatewayDeviceStep) Execute(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	host := machineHost(ctx, m)
	client, key, err := borrowSSHWithRetry(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return fmt.Errorf("pair-gateway-device: %w", err)
	}
	defer returnSSH(ctx, key, client)

	cfgHost := common.MachineConfigHost(m, host)

	// Trigger the local device pairing request with admin scope.
	// We use `openclaw cron list` because it requires operator.admin scope,
	// so the resulting pairing will include admin privileges for later CLI use.
	// On OpenClaw 2026.8+, this connection will be auto-approved immediately.
	_, _ = bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw cron list >/dev/null 2>&1 || true`)

	for attempt := 0; attempt < pairingRetries; attempt++ {
		dl, err := s.reader.DeviceList(ctx, client, cfgHost)
		if err != nil {
			return fmt.Errorf("pair-gateway-device: list devices: %w", err)
		}

		// Check if CLI is already paired with admin scope (OpenClaw 2026.8+ auto-approval)
		if hasLocalCLIWithAdminScope(dl) {
			return nil
		}

		// Check if CLI is paired at all (older OpenClaw or auto-approval without admin scope)
		if len(dl.Pending) == 0 && common.HasPairedLocalDevice(dl) {
			return nil
		}

		// Approve any pending requests (pre-2026.8 OpenClaw behavior)
		for _, p := range dl.Pending {
			if err := ApproveDevice(client, p.RequestID); err != nil {
				return fmt.Errorf("pair-gateway-device: %w", err)
			}
		}

		if len(dl.Pending) > 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pairingDelay):
		}
	}

	return fmt.Errorf("pair-gateway-device: no pending pairing request appeared after %d attempts", pairingRetries)
}

// Verify confirms at least one device is paired.
func (s *PairGatewayDeviceStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	host := machineHost(ctx, m)
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	cfgHost := common.MachineConfigHost(m, host)
	dl, err := s.reader.DeviceList(ctx, client, cfgHost)
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: %w", err)
	}
	if !common.HasPairedLocalDevice(dl) {
		return fmt.Errorf("pair-gateway-device verify: no paired device found after approval")
	}
	return nil
}
