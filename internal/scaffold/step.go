package scaffold

import "context"

// Barrier runs after all cells in a step finish.
type Barrier interface {
	Evaluate(ctx context.Context, stepName string, results []CellResult) error
}

// Step is a matrix: Targets x Actions.
type Step struct {
	Name    string
	Targets []Target
	Actions []Action
	Barrier Barrier
}

// AddTargets appends targets (fluent).
func (s *Step) AddTargets(ts ...Target) *Step {
	s.Targets = append(s.Targets, ts...)
	return s
}

// AddActions appends actions (fluent).
func (s *Step) AddActions(a ...Action) *Step {
	s.Actions = append(s.Actions, a...)
	return s
}

// SetBarrier sets the optional barrier.
func (s *Step) SetBarrier(b Barrier) *Step {
	s.Barrier = b
	return s
}
