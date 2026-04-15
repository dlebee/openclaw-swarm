package scaffold

import (
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

const defaultConcurrency = 4

// Plan is a mutable builder; call Build() for an ExecutablePlan.
type Plan struct {
	Phases []*Phase
}

// New creates an empty plan.
func New() *Plan {
	return &Plan{}
}

// AddPhase appends a phase and returns it for chaining.
func (p *Plan) AddPhase(name string) *Phase {
	ph := &Phase{Name: name, Concurrency: defaultConcurrency}
	p.Phases = append(p.Phases, ph)
	return ph
}

// Build validates, emits planning progress, and returns an ExecutablePlan.
func (p *Plan) Build(obs ...progress.BuildObserver) (*ExecutablePlan, error) {
	if len(p.Phases) == 0 {
		return nil, fmt.Errorf("scaffold: plan has no phases")
	}
	for pi, ph := range p.Phases {
		if ph.Name == "" {
			return nil, fmt.Errorf("scaffold: phase %d has empty name", pi)
		}
		if ph.Concurrency < 1 {
			return nil, fmt.Errorf("scaffold: phase %q: concurrency must be >= 1", ph.Name)
		}
		if len(ph.Steps) == 0 {
			return nil, fmt.Errorf("scaffold: phase %q has no steps", ph.Name)
		}
		for si, st := range ph.Steps {
			if st.Name == "" {
				return nil, fmt.Errorf("scaffold: phase %q step %d has empty name", ph.Name, si)
			}
			if len(st.Targets) == 0 {
				return nil, fmt.Errorf("scaffold: phase %q step %q has no targets", ph.Name, st.Name)
			}
			if len(st.Actions) == 0 {
				return nil, fmt.Errorf("scaffold: phase %q step %q has no actions", ph.Name, st.Name)
			}
			for _, a := range st.Actions {
				if a == nil {
					return nil, fmt.Errorf("scaffold: phase %q step %q has nil action", ph.Name, st.Name)
				}
				if a.Name() == "" {
					return nil, fmt.Errorf("scaffold: phase %q step %q has action with empty name", ph.Name, st.Name)
				}
			}
		}
	}
	compiled := compilePlan(p, obs)
	return &ExecutablePlan{compiled: compiled}, nil
}
