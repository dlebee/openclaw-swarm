// Package node is the node phase of the apply plan.
// Steps install and pair openclaw execution nodes with their gateways.
package node

import (
	"context"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const maxNodeConcurrency = 10

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc = common.SSHDialFunc

// NodeTarget is stored on scaffold.Target.Payload for node phase cells.
type NodeTarget struct {
	Spec    manifestdata.Node
	Machine manifestdata.Machine // resolved from Spec.Reference
	Gateway manifestdata.Gateway // resolved from Spec.Gateway
	GWMach  manifestdata.Machine // the gateway's machine (for SSH + internal_host)
}

// GetMachine implements common.MachineProvider.
func (nt *NodeTarget) GetMachine() manifestdata.Machine { return nt.Machine }

// GatewayInternalHost returns the address the node should use to connect to
// the gateway. Precedence:
//
//  1. Manifest-declared GWMach.InternalHost (explicit override — Docker
//     bridge, VPC, bastion, etc.)
//  2. Plan-cache mesh IP recorded by mesh.install-tailscale — required when
//     networking.mode selects a private overlay (headscale), because
//     openclaw's client refuses plaintext ws:// to public IPs ("SECURITY
//     ERROR: Cannot connect over plaintext"). The tailnet address (100.x/8
//     RFC6598) bypasses that guard AND is routable between mesh peers.
//  3. Manifest-declared GWMach.Host (static non-Linode gateway on a private
//     network the manifest already knows about).
//  4. Plan-cache public host recorded by provisioning.create-machine (last
//     resort — only works for loopback-bound gateways or when openclaw's
//     insecure-ws guard is satisfied some other way).
//
// Pre-cache fallback matters for standalone gateways (BYOS hosts that
// already have Host set in the manifest); for Linode-provisioned gateways
// the manifest Host is empty and the plan cache is the only source of truth.
func (nt *NodeTarget) GatewayInternalHost(ctx context.Context) string {
	if h := strings.TrimSpace(nt.GWMach.InternalHost); h != "" {
		return h
	}
	if ip, ok := scaffold.LookupPlanMachineMeshIP(ctx, nt.GWMach.Name); ok && ip != "" {
		return ip
	}
	if h := strings.TrimSpace(nt.GWMach.Host); h != "" {
		return h
	}
	return common.ResolveMachineHost(ctx, nt.GWMach)
}

// BuildNodeTargets creates scaffold targets from manifest nodes, resolving
// each node's Reference to its machine and Gateway to the gateway + gateway machine.
func BuildNodeTargets(nodes []manifestdata.Node, machines []manifestdata.Machine, gateways []manifestdata.Gateway) []scaffold.Target {
	machByName := make(map[string]manifestdata.Machine, len(machines))
	for _, m := range machines {
		machByName[m.Name] = m
	}
	gwByName := make(map[string]manifestdata.Gateway, len(gateways))
	for _, gw := range gateways {
		gwByName[gw.Name] = gw
	}

	targets := make([]scaffold.Target, 0, len(nodes))
	for _, n := range nodes {
		gw := gwByName[n.Gateway]
		targets = append(targets, scaffold.Target{
			ID: n.Name,
			Payload: &NodeTarget{
				Spec:    n,
				Machine: machByName[n.Reference],
				Gateway: gw,
				GWMach:  machByName[gw.Reference],
			},
		})
	}
	return targets
}

// Options configures the node phase.
type Options struct {
	SSHDial SSHDialFunc
}

// AddPhase registers the "node" phase.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, opts Options) *scaffold.Phase {
	ph := p.AddPhase("node")
	n := len(targets)
	if n < 1 {
		n = 1
	}
	if n > maxNodeConcurrency {
		n = maxNodeConcurrency
	}
	ph.Concurrency = n
	ph.AddTargets(targets...)
	ph.AddStep(common.NewInstallNodejsStep(common.Options{SSHDial: opts.SSHDial}))
	ph.AddStep(common.NewInstallOpenclawStep(common.Options{SSHDial: opts.SSHDial}))
	ph.AddStep(NewStubGatewayUnitStep(opts))
	ph.AddStep(NewBootstrapNodeStep(opts))
	ph.AddStep(NewConfigureNodeStep(opts))
	ph.AddStep(NewExecPolicyStep(opts))
	ph.AddStep(NewPairNodeStep(opts))
	return ph
}
