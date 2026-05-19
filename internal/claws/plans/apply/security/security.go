// Package security is the security phase of the apply plan.
// Steps: install-security-packages, enable-ufw, enable-fail2ban, enable-unattended-upgrades.
package security

import (
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const maxSecurityConcurrency = 5

// AddPhase registers the "security" phase. Targets run concurrently (capped
// at maxSecurityConcurrency); each target runs the four security steps
// sequentially. Targets must be the same []scaffold.Target slice used by
// provisioning so that Instance (with PublicIPv4) is already populated.
//
// Concurrency is sized to the number of machines the security phase will
// actually touch (hosted machines plus any opt-in `type: ssh` machines
// with apply_security: true). The same gate is enforced per-step in
// isSecurityApplicable so the count and the actual run set always match.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, opts Options) *scaffold.Phase {
	ph := p.AddPhase("security")

	applicableN := 0
	for _, t := range targets {
		if mt, ok := t.Payload.(*provisioning.MachineTarget); ok && mt.Spec.SecurityPhaseApplies() {
			applicableN++
		}
	}
	concurrency := applicableN
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > maxSecurityConcurrency {
		concurrency = maxSecurityConcurrency
	}
	ph.Concurrency = concurrency

	ph.AddTargets(targets...)

	ph.AddStep(NewInstallPackagesStep(opts))
	ph.AddStep(NewEnableUFWStep(opts))
	ph.AddStep(NewEnableFail2banStep(opts))
	ph.AddStep(NewEnableUnattendedUpgradesStep(opts))

	return ph
}
