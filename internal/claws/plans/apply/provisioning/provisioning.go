// Package provisioning is the provisioning phase of the apply plan (create-machine, then authorize-ssh-key).
package provisioning

import (
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const maxProvisioningConcurrency = 5

// AddPhase registers the "provisioning" phase: create-machine, then authorize-ssh-key (sequential steps).
// Concurrency is the number of Linode machines (minimum 1), capped at maxProvisioningConcurrency.
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

	targets := make([]scaffold.Target, 0, len(machines))
	for _, m := range machines {
		targets = append(targets, scaffold.Target{
			ID:      m.Name,
			Payload: &MachineTarget{Spec: m},
		})
	}

	sCreate := ph.AddStep("create-machine")
	sCreate.AddTargets(targets...).AddActions(NewCreateMachineAction(opts))

	sAuth := ph.AddStep("authorize-ssh-key")
	sAuth.AddTargets(targets...).AddActions(NewAuthorizeSSHKeyAction(opts))

	return ph
}
