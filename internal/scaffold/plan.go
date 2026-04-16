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
		if len(ph.Targets) == 0 {
			return nil, fmt.Errorf("scaffold: phase %q has no targets", ph.Name)
		}
		for _, s := range ph.Steps {
			if s == nil {
				return nil, fmt.Errorf("scaffold: phase %q has nil step", ph.Name)
			}
			if s.Name() == "" {
				return nil, fmt.Errorf("scaffold: phase %q has step with empty name", ph.Name)
			}
		}
	}
	compiled := compilePlan(p, obs)
	return &ExecutablePlan{compiled: compiled}, nil
}
