// Package provisioning is the provisioning phase of the apply plan
// (create-machine, authorize-ssh-key, ensure-agent-user).
package provisioning

import (
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const maxProvisioningConcurrency = 5

// BuildMachineTargets creates scaffold targets with shared *MachineTarget
// payloads. All phases that operate on machines should use the same targets so
// that Instance data populated by provisioning is visible to later phases.
func BuildMachineTargets(machines []manifestdata.Machine) []scaffold.Target {
	targets := make([]scaffold.Target, len(machines))
	for i, m := range machines {
		targets[i] = scaffold.Target{
			ID:      m.Name,
			Payload: &MachineTarget{Spec: m},
		}
	}
	return targets
}

// AddPhase registers the "provisioning" phase. Targets run concurrently (capped
// at maxProvisioningConcurrency); each target runs create-machine then
// authorize-ssh-key sequentially.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, opts Options) *scaffold.Phase {
	ph := p.AddPhase("provisioning")

	hostedN := 0
	for _, t := range targets {
		if mt, ok := t.Payload.(*MachineTarget); ok && manifestdata.IsHostedMachineType(mt.Spec.Type) {
			hostedN++
		}
	}
	concurrency := hostedN
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > maxProvisioningConcurrency {
		concurrency = maxProvisioningConcurrency
	}
	ph.Concurrency = concurrency

	ph.AddTargets(targets...)

	ph.AddStep(NewCreateMachineStep(opts))
	ph.AddStep(NewAuthorizeSSHKeyStep(opts))
	ph.AddStep(NewEnsureAgentUserStep(opts))

	return ph
}
