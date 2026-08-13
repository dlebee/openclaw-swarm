package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
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
//
// Flow:
// 1. Trigger pairing with `openclaw status` (minimal command, creates pairing request)
// 2. Poll for pending requests and approve them
// 3. Verify CLI is paired with admin scope
// 4. Run `openclaw cron list` to confirm admin access works
//
// This two-phase approach avoids the scope upgrade race condition where running
// an admin-scope command before pairing causes a separate scope upgrade request
// with a different requestId.
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

	// Phase 1: Trigger pairing with a minimal command.
	// `openclaw status` connects to the gateway and triggers a pairing request.
	// The CLI always requests operator.admin scope in its pairing request.
	_, _ = bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw status >/dev/null 2>&1 || true`)

	// Phase 2: Poll for pending requests and approve them.
	for attempt := 0; attempt < pairingRetries; attempt++ {
		dl, err := s.reader.DeviceList(ctx, client, cfgHost)
		if err != nil {
			return fmt.Errorf("pair-gateway-device: list devices: %w", err)
		}

		// Check if CLI is already paired with admin scope
		if hasLocalCLIWithAdminScope(dl) {
			return s.verifyAdminAccess(client)
		}

		// Approve any pending requests
		for _, p := range dl.Pending {
			if err := ApproveDevice(client, p.RequestID); err != nil {
				return fmt.Errorf("pair-gateway-device: %w", err)
			}
			// After approving, give gateway time to process, then verify
			time.Sleep(1 * time.Second)
			return s.verifyAdminAccess(client)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pairingDelay):
		}
	}

	return fmt.Errorf("pair-gateway-device: no pending pairing request appeared after %d attempts", pairingRetries)
}

// verifyAdminAccess confirms the CLI can run admin-scope commands.
func (s *PairGatewayDeviceStep) verifyAdminAccess(client *xssh.Client) error {
	// Run an admin-scope command to verify the pairing has full access.
	// This also warms up the CLI connection for subsequent commands.
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw cron list 2>&1`)
	if err != nil {
		return fmt.Errorf("pair-gateway-device: verify admin access: %w\n%s", err, out)
	}
	return nil
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
