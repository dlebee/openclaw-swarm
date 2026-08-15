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

// PairGatewayDeviceStep ensures the CLI can access the gateway with full
// admin scope. On loopback gateways with token auth, the CLI uses the
// config token directly (no device pairing needed). On gateways without
// shared-secret auth, the CLI connects with a device identity that must
// be paired and approved.
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

// Applicable returns true for any gateway target.
func (s *PairGatewayDeviceStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*GatewayTarget)
	return ok, nil
}

// Check verifies the CLI can run admin-scope commands on the gateway.
func (s *PairGatewayDeviceStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	host, ok := hostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	return cliHasAdminAccess(client), nil
}

// cliHasAdminAccess tests whether the CLI can run an admin-scope command.
// On loopback with token auth, the CLI uses the config token directly
// and bypasses device identity — so device pairing doesn't apply.
func cliHasAdminAccess(client *xssh.Client) bool {
	// `openclaw cron list` requires operator.read — any CLI with valid
	// auth can run it. But we need to verify admin access specifically.
	// We use `openclaw config get gateway.bind` which requires operator.admin.
	token := common.ReadGatewayAuthToken(client)
	var script string
	if token != "" {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(
			`openclaw config get gateway.bind --token %q 2>/dev/null`, token)
	} else {
		script = common.OpenclawCLIPreamble() + `openclaw config get gateway.bind 2>/dev/null`
	}
	_, err := bash.RunOutput(client, script)
	return err == nil
}

// Execute ensures the CLI has admin access to the gateway.
//
// With token auth on loopback (the standard swarm setup), the CLI uses
// the config token directly and skips device identity entirely. No
// device pairing is needed — we just verify the token works.
//
// Without token auth, the CLI sends a device identity that needs to be
// paired and approved. This path handles both cases.
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
	token := common.ReadGatewayAuthToken(client)

	// Fast path: if the CLI already has admin access (e.g. token auth
	// on loopback), no pairing is needed.
	if cliHasAdminAccess(client) {
		return nil
	}

	// Trigger a CLI connection to create the device identity and
	// initial pairing request. The gateway auto-pairs on loopback.
	_, _ = bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw cron list 2>/dev/null || true`)
	time.Sleep(1 * time.Second)

	// Check again after initial connection.
	if cliHasAdminAccess(client) {
		return nil
	}

	// Poll for pending requests and approve them.
	for attempt := 0; attempt < pairingRetries; attempt++ {
		dl, err := s.reader.DeviceList(ctx, client, cfgHost)
		if err != nil {
			return fmt.Errorf("pair-gateway-device: list devices: %w", err)
		}

		// Approve any pending requests using token auth.
		if len(dl.Pending) > 0 {
			for _, p := range dl.Pending {
				if err := approveDeviceRequest(client, p.RequestID, token); err != nil {
					return fmt.Errorf("pair-gateway-device: approve %s: %w", p.RequestID, err)
				}
			}
			time.Sleep(2 * time.Second)

			if cliHasAdminAccess(client) {
				return nil
			}
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pairingDelay):
		}
	}

	return fmt.Errorf("pair-gateway-device: CLI does not have admin access after %d attempts", pairingRetries)
}

// approveDeviceRequest approves a pending device pairing via the CLI.
// When a token is available, it uses --token --url to bypass device
// identity in the approval call itself.
func approveDeviceRequest(client *xssh.Client, requestID, token string) error {
	var script string
	if token != "" {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(
			`openclaw devices approve %q --token %q --url ws://127.0.0.1:18789 2>&1`,
			requestID, token)
	} else {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(
			`openclaw devices approve %q 2>&1`, requestID)
	}
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("%w\noutput: %s", err, out)
	}
	return nil
}

// Verify confirms the CLI has admin access.
func (s *PairGatewayDeviceStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	host := machineHost(ctx, m)
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if !cliHasAdminAccess(client) {
		return fmt.Errorf("pair-gateway-device verify: CLI does not have admin access")
	}
	return nil
}
