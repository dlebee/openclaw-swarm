package scaffold

import (
	"context"
)

// ExecutablePlan is the compiled result of Plan.Build().
type ExecutablePlan struct {
	compiled []compiledPhase
	preRun   func(ctx context.Context) error
}

// PhaseNames returns the ordered phase names in the compiled plan. Mirrors
// Plan.PhaseNames so callers holding only the ExecutablePlan (e.g. after
// ExecWithConfirm builds internally) can still validate phase filters.
func (e *ExecutablePlan) PhaseNames() []string {
	out := make([]string, 0, len(e.compiled))
	for _, ph := range e.compiled {
		out = append(out, ph.name)
	}
	return out
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

// Execute runs the plan. If a PreRun hook was attached via Plan.PreRun it is
// invoked after EnsurePlanCache and before the first phase runs, so the hook
// can seed plan-cache state (e.g. host / mesh-IP resolvers) that every phase
// may rely on regardless of phase filtering.
func (e *ExecutablePlan) Execute(ctx context.Context, opts ExecuteOptions) error {
	ctx = EnsurePlanCache(ctx)
	if e.preRun != nil {
		if err := e.preRun(ctx); err != nil {
			return err
		}
	}
	return runPlan(ctx, e.compiled, opts)
}
