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
// If already paired with limited scope, it directly patches the paired.json file
// to grant admin scope (the gateway will pick up the change on next CLI connect).
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

	// Check if the local CLI device is already paired with limited scope.
	// If so, patch the paired.json file directly to grant admin scope.
	dl, _ := s.reader.DeviceList(ctx, client, cfgHost)
	if dl != nil {
		for _, p := range dl.Paired {
			// Check if this is the local CLI device (clientId=cli, clientMode=cli)
			if p.ClientID == "cli" && p.ClientMode == "cli" {
				// Check if it has admin scope
				hasAdmin := false
				for _, scope := range p.Scopes {
					if scope == "operator.admin" {
						hasAdmin = true
						break
					}
				}
				if !hasAdmin {
					// Directly patch the paired.json file to add admin scope.
					// This is a workaround because the CLI can't call devices approve
					// without having scope, and we can't grant scope without calling
					// devices approve - chicken and egg.
					patchScript := `
jq --arg id "` + p.DeviceID + `" '
  if .[$id] then
    .[$id].scopes = ["operator.admin", "operator.read", "operator.write", "operator.pairing", "operator.approvals"] |
    .[$id].approvedScopes = ["operator.admin", "operator.read", "operator.write", "operator.pairing", "operator.approvals"]
  else . end
' ~/.openclaw/devices/paired.json > /tmp/paired.json.tmp && mv /tmp/paired.json.tmp ~/.openclaw/devices/paired.json
`
					_, _ = bash.RunOutput(client, patchScript)
					// Restart the gateway to pick up the change
					_, _ = bash.RunOutput(client, `sudo systemctl restart openclaw-gateway 2>/dev/null || true`)
					time.Sleep(3 * time.Second) // Give gateway time to restart
				}
			}
		}
	}

	// Trigger the local device pairing request with admin scope.
	// We use `openclaw cron list` because it requires operator.admin scope,
	// so the resulting pairing will include admin privileges for later CLI use.
	_, _ = bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw cron list >/dev/null 2>&1 || true`)

	for attempt := 0; attempt < pairingRetries; attempt++ {
		dl, err := s.reader.DeviceList(ctx, client, cfgHost)
		if err != nil {
			return fmt.Errorf("pair-gateway-device: list devices: %w", err)
		}

		if len(dl.Pending) == 0 && common.HasPairedLocalDevice(dl) {
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
