package gateway

import (
	"context"
	"encoding/json"
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
// On loopback gateways, the first CLI command that connects via WebSocket
// triggers silent auto-pairing with operator.admin scope. We run
// `openclaw cron list` as the trigger — it uses CLI mode (not probe mode),
// requests admin scope, and on loopback gets auto-approved immediately.
//
// If auto-pairing doesn't happen (e.g. non-loopback gateway or config
// differences), we fall back to polling for pending requests and approving
// them explicitly.
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

	// Try a CLI command first. On loopback this silently auto-pairs with admin.
	// Ignore errors — the command may fail if pairing requires approval.
	_, _ = bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw cron list 2>/dev/null || true`)

	// Give the gateway a moment to settle after the connect/pair.
	time.Sleep(1 * time.Second)

	// Debug: check what token we're reading
	token := common.ReadGatewayAuthToken(client)
	tokenLen := len(token)

	// Poll: check if auto-paired succeeded, otherwise approve pending requests.
	for attempt := 0; attempt < pairingRetries; attempt++ {
		dl, err := s.reader.DeviceList(ctx, client, cfgHost)
		if err != nil {
			return fmt.Errorf("pair-gateway-device: list devices: %w", err)
		}

		// Check if CLI is already paired with admin scope (auto-pair succeeded).
		if hasLocalCLIWithAdminScope(dl) {
			return s.verifyAdminAccess(client)
		}

		// If any paired CLI exists without admin, we need a scope upgrade.
		// If there are pending requests, approve them.
		approved := false
		for _, p := range dl.Pending {
			if err := ApproveDevice(client, p.RequestID); err != nil {
				// Include diagnostic info in error
				diag := fmt.Sprintf("tokenLen=%d, pending=%d, paired=%d", tokenLen, len(dl.Pending), len(dl.Paired))
				for i, pd := range dl.Paired {
					raw, _ := json.Marshal(pd)
					diag += fmt.Sprintf(", paired[%d]=%s", i, string(raw))
				}
				return fmt.Errorf("pair-gateway-device [%s]: %w", diag, err)
			}
			approved = true
		}

		if approved {
			time.Sleep(1 * time.Second)
			return s.verifyAdminAccess(client)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pairingDelay):
		}
	}

	return fmt.Errorf("pair-gateway-device: CLI device not paired with admin scope after %d attempts", pairingRetries)
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
