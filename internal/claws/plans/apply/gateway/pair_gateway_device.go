package gateway

import (
	"context"
	"fmt"
	"time"

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
	dial SSHDialFunc
}

func NewPairGatewayDeviceStep(opts Options) *PairGatewayDeviceStep {
	return &PairGatewayDeviceStep{dial: opts.SSHDial}
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
func (s *PairGatewayDeviceStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(ctx, m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	dl, err := ListDevices(client)
	if err != nil {
		return false, nil
	}
	return HasPairedLocalDevice(dl), nil
}

// Execute triggers a pairing request by calling `openclaw nodes list`
// (which seeds the pending device entry), then polls for and approves
// all pending requests.
func (s *PairGatewayDeviceStep) Execute(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSHWithRetry(ctx, s.dial, machineHost(ctx, m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("pair-gateway-device: %w", err)
	}
	defer returnSSH(ctx, key, client)

	// Trigger the local device pairing request.
	_, _ = bash.RunOutput(client, `openclaw nodes list >/dev/null 2>&1 || true`)

	for attempt := 0; attempt < pairingRetries; attempt++ {
		dl, err := ListDevices(client)
		if err != nil {
			return fmt.Errorf("pair-gateway-device: list devices: %w", err)
		}

		if len(dl.Pending) == 0 && HasPairedLocalDevice(dl) {
			return nil
		}

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
	client, key, err := borrowSSH(ctx, s.dial, machineHost(ctx, m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	dl, err := ListDevices(client)
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: %w", err)
	}
	if !HasPairedLocalDevice(dl) {
		return fmt.Errorf("pair-gateway-device verify: no paired device found after approval")
	}
	return nil
}
