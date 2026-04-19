package scaffold

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

const defaultConcurrency = 4

// Plan is a mutable builder; call Build() for an ExecutablePlan.
type Plan struct {
	Phases []*Phase

	// PreRun, if non-nil, is invoked by ExecutablePlan.Execute after
	// EnsurePlanCache and before any phase runs. Used by apply.BuildPlan to
	// install plan-scoped fallback resolvers (host resolver, mesh IP
	// resolver) so a cold `--only <phase>` invocation can lazily re-hydrate
	// state that would normally be populated by an earlier phase's cache
	// writes. Returning an error aborts the run before the first phase.
	PreRun func(ctx context.Context) error
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

// PhaseNames returns the ordered phase names as currently appended. Useful for
// validating user-supplied --phases / --skip-phases flags before Build.
func (p *Plan) PhaseNames() []string {
	out := make([]string, 0, len(p.Phases))
	for _, ph := range p.Phases {
		out = append(out, ph.Name)
	}
	return out
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
	return &ExecutablePlan{compiled: compiled, preRun: p.PreRun}, nil
}
