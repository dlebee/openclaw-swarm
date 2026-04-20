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

	// ProbeDependsOn names phases whose probe (Applicable+Check for every
	// cell in that phase) must finish before this phase's probe starts.
	//
	// Execute and barrier ordering are unchanged: phases still run in plan
	// append order during apply. Only the prepared-plan probe can schedule
	// independent phases in parallel when their dependency sets allow it.
	//
	// nil means "depend on the single immediately preceding phase in plan
	// order" (the first phase has no probe dependencies). A non-nil slice
	// is used verbatim; an empty slice means this phase's probe has no
	// dependencies (use sparingly).
	ProbeDependsOn []string

	// ProbeConcurrency caps how many targets probe concurrently
	// (Applicable+Check). Steps for a single target still run strictly in
	// order. Execute uses Concurrency instead; they are independent.
	// Zero means 1 (fully sequential target probe, legacy behavior).
	ProbeConcurrency int
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
