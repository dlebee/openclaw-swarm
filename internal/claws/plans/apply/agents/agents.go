// Package agents is the agents phase of the apply plan.
// Steps register and configure openclaw agents on their gateways.
package agents

import (
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// maxAgentConcurrency caps parallel agent targets. OpenClaw 2026.5.x
// fixed the race on openclaw.json writes, so we can now run agents
// in parallel.
const maxAgentConcurrency = 5

// maxAgentProbeConcurrency caps parallel Applicable+Check per target during
// prepared-plan probing only. Execute still uses maxAgentConcurrency.
const maxAgentProbeConcurrency = 5

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc = common.SSHDialFunc

// AgentTarget is stored on scaffold.Target.Payload for agent phase cells.
type AgentTarget struct {
	Spec    manifestdata.Agent   // manifest agent definition
	Gateway manifestdata.Gateway // resolved gateway
	Machine manifestdata.Machine // gateway's machine
}

// BuildAgentTargets creates scaffold targets from manifest agents, resolving
// each agent's Gateway to the gateway spec and its machine.
func BuildAgentTargets(agents []manifestdata.Agent, gateways []manifestdata.Gateway, machines []manifestdata.Machine) []scaffold.Target {
	gwByName := make(map[string]manifestdata.Gateway, len(gateways))
	for _, gw := range gateways {
		gwByName[gw.Name] = gw
	}
	machByName := make(map[string]manifestdata.Machine, len(machines))
	for _, m := range machines {
		machByName[m.Name] = m
	}

	targets := make([]scaffold.Target, 0, len(agents))
	for _, a := range agents {
		gw := gwByName[a.Gateway]
		targets = append(targets, scaffold.Target{
			ID: a.ID,
			Payload: &AgentTarget{
				Spec:    a,
				Gateway: gw,
				Machine: machByName[gw.Reference],
			},
		})
	}
	return targets
}

// Options configures the agents phase.
type Options struct {
	SSHDial SSHDialFunc
	// ConfigReader is consulted by every step's Check to probe gateway
	// state. When nil, AddPhase installs the default snapshot-over-CLI
	// reader, which caches a single ~/.openclaw/openclaw.json read per
	// gateway for the entire probe pass and transparently falls through
	// to the CLI during Execute. Tests can inject a fake reader here.
	ConfigReader common.ConfigReader
}

// AddPhase registers the "agents" phase.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, opts Options) *scaffold.Phase {
	if opts.ConfigReader == nil {
		opts.ConfigReader = common.DefaultConfigReader(opts.SSHDial)
	}
	ph := p.AddPhase("agents")
	ph.Concurrency = maxAgentConcurrency
	nProbe := len(targets)
	if nProbe < 1 {
		nProbe = 1
	}
	if nProbe > maxAgentProbeConcurrency {
		nProbe = maxAgentProbeConcurrency
	}
	ph.ProbeConcurrency = nProbe
	ph.AddTargets(targets...)
	ph.AddStep(NewAddAgentStep(opts))
	ph.AddStep(NewEnsureModelStep(opts))
	ph.AddStep(NewConfigureWorkspaceStep(opts))
	ph.AddStep(NewConfigureToolsStep(opts))
	ph.AddStep(NewConfigureBindingsStep(opts))
	return ph
}
