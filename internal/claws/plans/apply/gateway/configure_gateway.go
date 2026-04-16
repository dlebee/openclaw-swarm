package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ConfigureGatewayStep repairs drift on an already-onboarded gateway
// (gateway.mode, gateway.bind, insecure WS env in the systemd unit).
// Not applicable when the gateway has not been bootstrapped yet.
type ConfigureGatewayStep struct {
	dial SSHDialFunc
}

func NewConfigureGatewayStep(opts Options) *ConfigureGatewayStep {
	return &ConfigureGatewayStep{dial: opts.SSHDial}
}

func (*ConfigureGatewayStep) Name() string { return "configure-gateway" }

// Applicable returns true for any gateway target. The step runs after
// bootstrap-gateway in the pipeline, so by execution time the config will
// exist. Check determines whether there is actual drift to repair.
func (s *ConfigureGatewayStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*GatewayTarget)
	return ok, nil
}

// Check reads the current gateway.mode and gateway.bind from the remote
// config and compares them against the desired values derived from the
// manifest. Returns true (satisfied) when no drift is detected.
func (s *ConfigureGatewayStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	currentMode, err := ReadConfigValue(client, "gateway.mode")
	if err != nil {
		return false, nil
	}
	currentBind, err := ReadConfigValue(client, "gateway.bind")
	if err != nil {
		return false, nil
	}

	desiredBind := DesiredBind(gt.Spec)
	if currentMode != "local" || currentBind != desiredBind {
		return false, nil
	}

	desiredEnv := gatewayEnv(gt.Spec)
	currentEnv, err := systemd.ReadEnvDropIn(client, gatewayUnit, true)
	if err != nil {
		return false, nil
	}
	for k, v := range desiredEnv {
		if currentEnv[k] != v {
			return false, nil
		}
	}

	return true, nil
}

type batchEntry struct {
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// gatewayEnv returns the desired systemd environment variables for the
// gateway unit based on the manifest networking configuration.
func gatewayEnv(gw manifestdata.Gateway) map[string]string {
	env := make(map[string]string)
	if NeedsInsecureWS(gw) {
		env["OPENCLAW_ALLOW_INSECURE_PRIVATE_WS"] = "1"
	}
	return env
}

// Execute applies the correct gateway.mode and gateway.bind via
// `openclaw config set --batch-json`, then restarts the service.
func (s *ConfigureGatewayStep) Execute(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSHWithRetry(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("configure-gateway: %w", err)
	}
	defer returnSSH(ctx, key, client)

	desiredBind := DesiredBind(gt.Spec)
	batch := []batchEntry{
		{Path: "gateway.mode", Value: "local"},
		{Path: "gateway.bind", Value: desiredBind},
	}
	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("configure-gateway: marshal batch: %w", err)
	}

	script := fmt.Sprintf(`set -euo pipefail
openclaw config set --batch-json '%s'
`, string(batchJSON))

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("configure-gateway: config set: %w\n%s", err, out)
	}

	desiredEnv := gatewayEnv(gt.Spec)
	if err := systemd.WriteEnvDropIn(client, gatewayUnit, true, desiredEnv); err != nil {
		return fmt.Errorf("configure-gateway: write env drop-in: %w", err)
	}

	if err := RestartAndWait(ctx, client, true); err != nil {
		return fmt.Errorf("configure-gateway: %w", err)
	}

	return nil
}

// Verify re-reads the config values and confirms they match the desired state.
func (s *ConfigureGatewayStep) Verify(ctx context.Context, t scaffold.Target) error {
	gt := t.Payload.(*GatewayTarget)
	m := gt.Machine
	client, key, err := borrowSSH(ctx, s.dial, machineHost(m), machineSSHPort(m), machineSSHUser(m))
	if err != nil {
		return fmt.Errorf("configure-gateway verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	currentMode, err := ReadConfigValue(client, "gateway.mode")
	if err != nil {
		return fmt.Errorf("configure-gateway verify: read mode: %w", err)
	}
	currentBind, err := ReadConfigValue(client, "gateway.bind")
	if err != nil {
		return fmt.Errorf("configure-gateway verify: read bind: %w", err)
	}

	desiredBind := DesiredBind(gt.Spec)
	var drifts []string
	if currentMode != "local" {
		drifts = append(drifts, fmt.Sprintf("gateway.mode=%q want local", currentMode))
	}
	if currentBind != desiredBind {
		drifts = append(drifts, fmt.Sprintf("gateway.bind=%q want %q", currentBind, desiredBind))
	}

	desiredEnv := gatewayEnv(gt.Spec)
	currentEnv, _ := systemd.ReadEnvDropIn(client, gatewayUnit, true)
	for k, v := range desiredEnv {
		if currentEnv[k] != v {
			drifts = append(drifts, fmt.Sprintf("env %s=%q want %q", k, currentEnv[k], v))
		}
	}

	if len(drifts) > 0 {
		return fmt.Errorf("configure-gateway verify: still drifted: %s", strings.Join(drifts, "; "))
	}
	return nil
}
