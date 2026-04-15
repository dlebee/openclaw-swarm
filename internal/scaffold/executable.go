package scaffold

import (
	"context"
)

// ExecutablePlan is the compiled result of Plan.Build().
type ExecutablePlan struct {
	compiled []compiledPhase
}

// Describe returns a plain-text outline (no ANSI).
func (e *ExecutablePlan) Describe() string {
	return describePlain(e.compiled)
}

// DescribeStyled returns a Lip Gloss styled outline.
func (e *ExecutablePlan) DescribeStyled(width int) string {
	return describeStyled(e.compiled, width)
}

// Execute runs the plan.
func (e *ExecutablePlan) Execute(ctx context.Context, opts ExecuteOptions) error {
	return runPlan(ctx, e.compiled, opts)
}
