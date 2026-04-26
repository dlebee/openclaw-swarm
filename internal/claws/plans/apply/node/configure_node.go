package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const nodeUnit = "openclaw-node"

// ConfigureNodeStep ensures the node's systemd environment drop-in is in sync
// with the manifest. Repairs drift after bootstrap.
//
// The gateway token is baked into the unit's Environment= by `openclaw node
// install`, so we don't manage it here. This step only manages extra env vars
// like NODE_COMPILE_CACHE that the upstream install doesn't capture.
//
// Note: OPENCLAW_ALLOW_INSECURE_PRIVATE_WS is now captured by upstream
// `openclaw node install` when set in the install environment, so we no
// longer need to manage it here.
type ConfigureNodeStep struct {
	dial         SSHDialFunc
	hostResolver common.HostResolverFn
}

func NewConfigureNodeStep(opts Options) *ConfigureNodeStep {
	return &ConfigureNodeStep{dial: opts.SSHDial, hostResolver: opts.HostResolver}
}

func (*ConfigureNodeStep) Name() string { return "configure-node" }

func (s *ConfigureNodeStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*NodeTarget)
	return ok, nil
}

func (s *ConfigureNodeStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	host, ok := common.HostKnown(ctx, m, s.hostResolver)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	desiredEnv := nodeEnv(nt)
	if len(desiredEnv) == 0 {
		return true, nil
	}

	currentEnv, err := systemd.ReadEnvDropIn(client, nodeUnit, true)
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

// nodeEnv returns the desired systemd environment variables for the
// openclaw-node unit. Always includes the openclaw startup-optimisation env
// (NODE_COMPILE_CACHE, OPENCLAW_NO_RESPAWN) — the node daemon respawns
// short-lived workers per tool invocation, so a persistent V8 compile cache
// materially cuts exec latency.
func nodeEnv(nt *NodeTarget) map[string]string {
	// OPENCLAW_ALLOW_INSECURE_PRIVATE_WS is now captured by upstream
	// `openclaw node install` when set in the install environment
	// (bootstrap_node.go sets it via envPrefix), so we only need the
	// startup optimization env vars here.
	return gwService.StartupOptimEnv()
}

func (s *ConfigureNodeStep) Execute(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-node: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// NODE_COMPILE_CACHE is set in the drop-in below; the target directory
	// must exist before the unit starts or Node silently disables caching.
	if err := gwService.EnsureNodeCompileCacheDir(client); err != nil {
		return fmt.Errorf("configure-node: %w", err)
	}

	desiredEnv := nodeEnv(nt)
	if err := systemd.WriteEnvDropIn(client, nodeUnit, true, desiredEnv); err != nil {
		return fmt.Errorf("configure-node: write env drop-in: %w", err)
	}

	if err := systemd.Restart(client, nodeUnit, true); err != nil {
		return fmt.Errorf("configure-node: restart: %w", err)
	}

	return nil
}

func (s *ConfigureNodeStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-node verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	desiredEnv := nodeEnv(nt)
	currentEnv, _ := systemd.ReadEnvDropIn(client, nodeUnit, true)
	var drifts []string
	for k, v := range desiredEnv {
		if currentEnv[k] != v {
			drifts = append(drifts, fmt.Sprintf("env %s=%q want %q", k, currentEnv[k], v))
		}
	}
	if len(drifts) > 0 {
		return fmt.Errorf("configure-node verify: drift: %s", strings.Join(drifts, "; "))
	}

	return nil
}
