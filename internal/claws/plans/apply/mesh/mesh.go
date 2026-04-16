// Package mesh is the mesh-networking phase of the apply plan.
// Steps install Headscale, Caddy, resolve preauth keys, and join Tailscale.
package mesh

import (
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// SSHDialFunc is an alias for the common SSH dial function type.
type SSHDialFunc = common.SSHDialFunc

// MeshTarget is stored on scaffold.Target.Payload for mesh phase cells.
type MeshTarget struct {
	Machine       manifestdata.Machine
	Gateway       *manifestdata.Gateway // non-nil only for the gateway reference machine
	IsGatewayHost bool
}

// GetMachine implements common.MachineProvider.
func (mt *MeshTarget) GetMachine() manifestdata.Machine { return mt.Machine }

// Options configures the mesh phase.
type Options struct {
	SSHDial  SSHDialFunc
	Machines []manifestdata.Machine
	Gateways []manifestdata.Gateway
	Nodes    []manifestdata.Node
}

// BuildMeshTargets creates scaffold targets for all machines that participate
// in headscale mesh networking. A machine participates if it is the reference
// for a headscale gateway, or a node whose gateway uses headscale.
func BuildMeshTargets(machines []manifestdata.Machine, gateways []manifestdata.Gateway, nodes []manifestdata.Node) []scaffold.Target {
	machByName := make(map[string]manifestdata.Machine, len(machines))
	for _, m := range machines {
		machByName[m.Name] = m
	}

	gwByRef := make(map[string]*manifestdata.Gateway)
	for i := range gateways {
		gw := &gateways[i]
		if !isHeadscale(gw) {
			continue
		}
		gwByRef[gw.Reference] = gw
	}

	// No headscale gateways → no mesh targets.
	if len(gwByRef) == 0 {
		return nil
	}

	// Collect headscale gateway names for node lookup.
	hsGWNames := make(map[string]bool, len(gwByRef))
	for _, gw := range gwByRef {
		hsGWNames[gw.Name] = true
	}

	// Deduplicate: a machine may be both a gateway host and a node host.
	seen := make(map[string]bool)
	var targets []scaffold.Target

	// Gateway reference machines first.
	for ref, gw := range gwByRef {
		mach, ok := machByName[ref]
		if !ok {
			continue
		}
		seen[mach.Name] = true
		targets = append(targets, scaffold.Target{
			ID: mach.Name,
			Payload: &MeshTarget{
				Machine:       mach,
				Gateway:       gw,
				IsGatewayHost: true,
			},
		})
	}

	// Node machines whose gateway is headscale.
	for _, n := range nodes {
		if !hsGWNames[n.Gateway] {
			continue
		}
		if seen[n.Reference] {
			continue
		}
		mach, ok := machByName[n.Reference]
		if !ok {
			continue
		}
		seen[mach.Name] = true
		targets = append(targets, scaffold.Target{
			ID: mach.Name,
			Payload: &MeshTarget{
				Machine: mach,
			},
		})
	}

	return targets
}

// AddPhase registers the mesh phases. Two phases are used so that gateway
// setup (resolve URL, install headscale/caddy, resolve preauth key) always
// completes before install-tailscale runs on any target:
//
//   - "mesh-gateway": gateway target only; runs control-plane setup steps.
//   - "mesh-join":    all mesh targets; joins every machine to the mesh.
//
// If no headscale targets exist, a single noop "configure-mesh" phase is
// added so the phase slot still appears in the plan.
func AddPhase(p *scaffold.Plan, machineTargets []scaffold.Target, opts Options) *scaffold.Phase {
	machines := opts.Machines
	if len(machines) == 0 {
		machines = extractMachines(machineTargets)
	}
	meshTargets := BuildMeshTargets(
		machines,
		opts.Gateways,
		opts.Nodes,
	)

	if len(meshTargets) == 0 {
		ph := p.AddPhase("mesh")
		ph.AddTargets(machineTargets...)
		ph.AddStep(scaffold.NoopStep{StepName: "configure-mesh"})
		return ph
	}

	// Phase 1: gateway-only control-plane setup.
	var gatewayTargets []scaffold.Target
	for _, t := range meshTargets {
		if mt, ok := t.Payload.(*MeshTarget); ok && mt.IsGatewayHost {
			gatewayTargets = append(gatewayTargets, t)
		}
	}
	phGW := p.AddPhase("mesh-gateway")
	phGW.AddTargets(gatewayTargets...)
	phGW.AddStep(NewResolveControlURLStep(opts))
	phGW.AddStep(NewInstallHeadscaleStep(opts))
	phGW.AddStep(NewInstallCaddyStep(opts))
	phGW.AddStep(NewResolvePreauthKeyStep(opts))

	// Phase 2: join all mesh targets (gateway + nodes).
	phJoin := p.AddPhase("mesh-join")
	phJoin.AddTargets(meshTargets...)
	phJoin.AddStep(NewInstallTailscaleStep(opts))
	return phJoin
}

func isHeadscale(gw *manifestdata.Gateway) bool {
	if gw == nil || gw.Networking == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(gw.Networking.Mode), "headscale")
}

func extractMachines(targets []scaffold.Target) []manifestdata.Machine {
	machines := make([]manifestdata.Machine, 0, len(targets))
	for _, t := range targets {
		if mp, ok := t.Payload.(common.MachineProvider); ok {
			machines = append(machines, mp.GetMachine())
		}
	}
	return machines
}
