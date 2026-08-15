package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

const (
	pairingRetries = 12
	pairingDelay   = 3 * time.Second
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

// Check verifies the CLI can run commands against the gateway.
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

	_, err = runCLICommand(client)
	return err == nil, nil
}

// runCLICommand runs a lightweight gateway RPC command via the CLI.
// On loopback with token auth, the CLI uses the config token directly
// and bypasses device identity — so device pairing doesn't apply.
// Returns the command output and any error.
func runCLICommand(client *xssh.Client) (string, error) {
	token := common.ReadGatewayAuthToken(client)
	var script string
	if token != "" {
		// Explicit --url ensures loopback detection. --token provides auth.
		// Together they make the CLI skip config loading and omit device identity.
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(
			`openclaw cron list --token %q --url ws://127.0.0.1:18789 2>&1`, token)
	} else {
		script = common.OpenclawCLIPreamble() + `openclaw cron list 2>&1`
	}
	return bash.RunOutput(client, script)
}

// Execute ensures the CLI has admin access to the gateway.
//
// With token auth on loopback (the standard swarm setup), the CLI uses
// the config token directly and skips device identity entirely. No
// device pairing is needed — we just verify the token works.
//
// The gateway might still be initializing after onboard, so we retry
// with increasing delays to wait for readiness.
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
	var lastErr string

	for attempt := 0; attempt < pairingRetries; attempt++ {
		out, err := runCLICommand(client)
		if err == nil {
			return nil // CLI can talk to the gateway — done
		}
		lastErr = strings.TrimSpace(out)

		// If there are pending device pairing requests, approve them.
		// This handles the non-token-auth case where device identity is used.
		dl, dlErr := s.reader.DeviceList(ctx, client, cfgHost)
		if dlErr == nil && dl != nil && len(dl.Pending) > 0 {
			for _, p := range dl.Pending {
				_ = approveDeviceRequest(client, p.RequestID, token)
			}
			time.Sleep(2 * time.Second)
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pairingDelay):
		}
	}

	// Grab gateway logs for diagnostics.
	logs, _ := bash.RunOutput(client, strings.Join([]string{
		`export XDG_RUNTIME_DIR=/run/user/$(id -u)`,
		`journalctl --user -u openclaw-gateway -n 30 --no-pager 2>/dev/null || true`,
	}, "\n"))

	return fmt.Errorf("pair-gateway-device: CLI cannot access gateway after %d attempts (tokenLen=%d)\nlast error: %s\ngateway logs:\n%s",
		pairingRetries, len(token), lastErr, logs)
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

// Verify confirms the CLI can access the gateway.
func (s *PairGatewayDeviceStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	host := machineHost(ctx, m)
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	_, err = runCLICommand(client)
	if err != nil {
		return fmt.Errorf("pair-gateway-device verify: CLI cannot access gateway")
	}
	return nil
}
