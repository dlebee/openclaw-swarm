// Package gateway is the gateway phase of the apply plan.
// Steps configure openclaw gateway instances on their referenced machines.
package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

const maxGatewayConcurrency = 5

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc func(ctx context.Context, host string, port int, user string) (*xssh.Client, error)

// GatewayTarget is stored on scaffold.Target.Payload for gateway phase cells.
type GatewayTarget struct {
	Spec    manifestdata.Gateway
	Machine manifestdata.Machine
}

// GetMachine implements common.MachineProvider.
func (gt *GatewayTarget) GetMachine() manifestdata.Machine { return gt.Machine }

// BuildGatewayTargets creates scaffold targets from manifest gateways, resolving
// each gateway's Reference to its machine.
func BuildGatewayTargets(gateways []manifestdata.Gateway, machines []manifestdata.Machine) []scaffold.Target {
	byName := make(map[string]manifestdata.Machine, len(machines))
	for _, m := range machines {
		byName[m.Name] = m
	}
	targets := make([]scaffold.Target, len(gateways))
	for i, gw := range gateways {
		targets[i] = scaffold.Target{
			ID: gw.Name,
			Payload: &GatewayTarget{
				Spec:    gw,
				Machine: byName[gw.Reference],
			},
		}
	}
	return targets
}

// Options configures the gateway phase.
type Options struct {
	SSHDial SSHDialFunc
}

// AddPhase registers the "gateway" phase with up to 5 concurrent targets.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, opts Options) *scaffold.Phase {
	ph := p.AddPhase("gateway")
	n := len(targets)
	if n < 1 {
		n = 1
	}
	if n > maxGatewayConcurrency {
		n = maxGatewayConcurrency
	}
	ph.Concurrency = n
	ph.AddTargets(targets...)
	ph.AddStep(common.NewInstallNodejsStep(common.Options{SSHDial: common.SSHDialFunc(opts.SSHDial)}))
	ph.AddStep(common.NewInstallOpenclawStep(common.Options{SSHDial: common.SSHDialFunc(opts.SSHDial)}))
	ph.AddStep(NewBootstrapGatewayStep(opts))
	ph.AddStep(NewConfigureGatewayStep(opts))
	ph.AddStep(NewPairGatewayDeviceStep(opts))
	return ph
}

// ---------------------------------------------------------------------------
// SSH helpers
// ---------------------------------------------------------------------------

const (
	sshDialRetries    = 10
	sshDialRetryDelay = 3 * time.Second
)

// machineHost returns the reachable SSH host for m, preferring the plan-cache
// entry recorded by create-machine (for Linode instances with dynamic IPs)
// over the manifest's static Spec.Host. Delegates to common.ResolveMachineHost
// so gateway-phase steps see the same cache entries as every other phase.
func machineHost(ctx context.Context, m manifestdata.Machine) string {
	return common.ResolveMachineHost(ctx, m)
}

func machineSSHPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

// machineSSHUser delegates to common.MachineSSHUser so the gateway phase
// follows the same SSHUser → AgentUser → root precedence as the rest of
// the apply plan.
func machineSSHUser(m manifestdata.Machine) string {
	return common.MachineSSHUser(m)
}

func borrowSSH(ctx context.Context, dial SSHDialFunc, host string, port int, user string) (*xssh.Client, string, error) {
	key := clawssh.HostKey(host, port, user)
	if pool := sshPool(ctx); pool != nil {
		c, err := pool.Borrow(ctx, key, func(ctx context.Context) (*xssh.Client, error) {
			return dial(ctx, host, port, user)
		})
		return c, key, err
	}
	c, err := dial(ctx, host, port, user)
	return c, key, err
}

func borrowSSHWithRetry(ctx context.Context, dial SSHDialFunc, host string, port int, user string) (*xssh.Client, string, error) {
	var lastErr error
	for attempt := 0; attempt < sshDialRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		c, key, err := borrowSSH(ctx, dial, host, port, user)
		if err == nil {
			return c, key, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(sshDialRetryDelay):
		}
	}
	return nil, "", fmt.Errorf("dial %s@%s:%d after %d retries: %w", user, host, port, sshDialRetries, lastErr)
}

func returnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := sshPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}

func sshPool(ctx context.Context) *clawssh.Pool {
	return provisioning.SSHPool(ctx)
}
