// Package mesh is the mesh-networking phase of the apply plan.
// Placeholder — steps will be added when mesh support is implemented.
package mesh

import (
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// Options configures the mesh phase.
type Options struct{}

// AddPhase registers the "mesh" phase. Currently a placeholder — no real steps.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, _ Options) *scaffold.Phase {
	ph := p.AddPhase("mesh")
	ph.Concurrency = 1
	ph.AddTargets(targets...)
	ph.AddStep(scaffold.NoopStep{StepName: "configure-mesh"})
	return ph
}
