package scaffold

import (
	"context"
)

// ExecutablePlan is the compiled result of Plan.Build().
type ExecutablePlan struct {
	compiled []compiledPhase
}

// Describe returns a plain-text outline (no ANSI) after Applicable+Check per cell.
func (e *ExecutablePlan) Describe(ctx context.Context) (string, error) {
	ctx = EnsurePlanCache(ctx)
	return describePlainWithProbe(ctx, e.compiled, PlanDisplayHints{})
}

// DescribeStyled returns a Lip Gloss tree after Applicable+Check (default hints).
func (e *ExecutablePlan) DescribeStyled(ctx context.Context, width int) (string, error) {
	return e.DescribeStyledWithHints(ctx, width, PlanDisplayHints{})
}

// DescribeStyledWithHints renders the probed plan tree. SkipPhases skips probing
// for those phases; DryRun only changes "would execute" vs "will execute" labels.
func (e *ExecutablePlan) DescribeStyledWithHints(ctx context.Context, width int, h PlanDisplayHints) (string, error) {
	ctx = EnsurePlanCache(ctx)
	cells, err := annotatePlanCellsWithProbe(ctx, e.compiled, h)
	if err != nil {
		return "", err
	}
	return renderPreparedPlanTree(cells, width, h), nil
}

// Execute runs the plan.
func (e *ExecutablePlan) Execute(ctx context.Context, opts ExecuteOptions) error {
	return runPlan(ctx, e.compiled, opts)
}
