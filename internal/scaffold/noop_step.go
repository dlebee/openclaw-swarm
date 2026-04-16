package scaffold

import "context"

// NoopStep is a placeholder step that is never applicable.
// Use it to satisfy the "phase must have at least one step" constraint
// for phases that don't have real steps yet.
type NoopStep struct {
	StepName string
}

func (s NoopStep) Name() string { return s.StepName }

func (NoopStep) Applicable(context.Context, Target) (bool, error) { return false, nil }
func (NoopStep) Check(context.Context, Target) (bool, error)      { return false, nil }
func (NoopStep) Execute(context.Context, Target) error             { return nil }
func (NoopStep) Verify(context.Context, Target) error              { return nil }
