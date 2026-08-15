package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ConfigureGatewayStep repairs drift on an already-onboarded gateway
// (gateway.mode, gateway.bind, insecure WS env in the systemd unit).
// Not applicable when the gateway has not been bootstrapped yet.
type ConfigureGatewayStep struct {
	dial   SSHDialFunc
	reader common.ConfigReader
}

func NewConfigureGatewayStep(opts Options) *ConfigureGatewayStep {
	r := opts.ConfigReader
	if r == nil {
		r = common.DefaultConfigReader(common.SSHDialFunc(opts.SSHDial))
	}
	return &ConfigureGatewayStep{dial: opts.SSHDial, reader: r}
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
	view, err := s.reader.GatewayView(ctx, client, cfgHost)
	if err != nil {
		return false, fmt.Errorf("read gateway view on %s: %w", m.Name, err)
	}

	desiredBind := DesiredBind(gt.Spec)
	if view.Mode != "local" || view.Bind != desiredBind {
		return false, nil
	}

	desiredEnv := gatewayEnv(gt.Spec)
	currentEnv, err := systemd.ReadEnvDropIn(client, gatewayUnit, true)
	if err != nil {
		return false, fmt.Errorf("read env drop-in on %s: %w", m.Name, err)
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
// gateway unit. Always includes openclaw's recommended startup-optimisation
// env (NODE_COMPILE_CACHE, OPENCLAW_NO_RESPAWN) so isolated cron runs and
// other short-lived node subprocesses spawned by the gateway don't pay full
// cold-start cost every invocation.
func gatewayEnv(gw manifestdata.Gateway) map[string]string {
	env := StartupOptimEnv()
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
	client, key, err := borrowSSHWithRetry(ctx, s.dial, machineHost(ctx, m), machineSSHPort(m), machineAgentUser(m))
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

	// Use --url to ensure loopback detection so the CLI omits device identity.
	// Without --url the CLI may create a device pairing on the first call.
	token := common.ReadGatewayAuthToken(client)
	var configSetScript string
	if token != "" {
		configSetScript = fmt.Sprintf(`openclaw config set --batch-json '%s' --token %q --url ws://127.0.0.1:18789`, string(batchJSON), token)
	} else {
		configSetScript = fmt.Sprintf(`openclaw config set --batch-json '%s'`, string(batchJSON))
	}
	script := fmt.Sprintf(`set -euo pipefail
%s%s
`, common.OpenclawCLIPreamble(), configSetScript)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("configure-gateway: config set: %w\n%s", err, out)
	}

	// NODE_COMPILE_CACHE lands in the drop-in below; the directory has to
	// exist on disk before the unit starts or Node silently skips caching.
	if err := EnsureNodeCompileCacheDir(client); err != nil {
		return fmt.Errorf("configure-gateway: %w", err)
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
	host := machineHost(ctx, m)
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(m), machineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-gateway verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	// Verify runs outside the probe phase, so the snapshot reader
	// transparently delegates to the CLI — we see the writes Execute
	// just made rather than a stale cached snapshot.
	cfgHost := common.MachineConfigHost(m, host)
	view, err := s.reader.GatewayView(ctx, client, cfgHost)
	if err != nil {
		return fmt.Errorf("configure-gateway verify: read gateway view: %w", err)
	}

	desiredBind := DesiredBind(gt.Spec)
	var drifts []string
	if view.Mode != "local" {
		drifts = append(drifts, fmt.Sprintf("gateway.mode=%q want local", view.Mode))
	}
	if view.Bind != desiredBind {
		drifts = append(drifts, fmt.Sprintf("gateway.bind=%q want %q", view.Bind, desiredBind))
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
