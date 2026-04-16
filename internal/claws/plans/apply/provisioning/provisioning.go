// Package provisioning is the provisioning phase of the apply plan (create-machine, then authorize-ssh-key).
package provisioning

import (
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const maxProvisioningConcurrency = 5

// AddPhase registers the "provisioning" phase. Targets run concurrently (capped
// at maxProvisioningConcurrency); each target runs create-machine then
// authorize-ssh-key sequentially.
func AddPhase(p *scaffold.Plan, machines []manifestdata.Machine, opts Options) *scaffold.Phase {
	ph := p.AddPhase("provisioning")

	linodeN := 0
	for _, m := range machines {
		if m.Type == manifestdata.MachineTypeLinode {
			linodeN++
		}
	}
	concurrency := linodeN
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > maxProvisioningConcurrency {
		concurrency = maxProvisioningConcurrency
	}
	ph.Concurrency = concurrency

	for _, m := range machines {
		ph.AddTargets(scaffold.Target{
			ID:      m.Name,
			Payload: &MachineTarget{Spec: m},
		})
	}

	ph.AddStep(NewCreateMachineStep(opts))
	ph.AddStep(NewAuthorizeSSHKeyStep(opts))

	return ph
}
