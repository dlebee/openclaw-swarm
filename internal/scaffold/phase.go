package scaffold

import "context"

// Phase is an ordered group of targets and steps.
// All targets run concurrently (up to Concurrency); each target runs
// its steps sequentially. An optional Barrier evaluates after all targets
// complete; if it fails the plan stops before the next phase.
type Phase struct {
	Name        string
	Targets     []Target
	Steps       []Step
	Concurrency int
	Barrier     Barrier
}

// AddStep registers a step to run for every target in the phase.
func (p *Phase) AddStep(s Step) {
	p.Steps = append(p.Steps, s)
}

// AddTargets appends targets to the phase.
func (p *Phase) AddTargets(ts ...Target) {
	p.Targets = append(p.Targets, ts...)
}

// Barrier runs after all targets in a phase finish.
type Barrier interface {
	Evaluate(ctx context.Context, phaseName string, results []CellResult) error
}
