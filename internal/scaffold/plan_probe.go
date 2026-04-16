package scaffold

import (
	"context"
)

// annotatePlanCellsWithProbe walks the compiled plan in execution order, runs
// Applicable then Check for each cell (unless the phase is skipped), and builds
// rows for the prepared-plan tree. Execute and Verify are not called — Verify
// only exists on the real execution path after Execute.
func annotatePlanCellsWithProbe(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints) ([]annotatedCell, error) {
	var out []annotatedCell
	seq := 0
	for _, ph := range compiled {
		if skipName(ph.name, h.SkipPhases) {
			for _, st := range ph.steps {
				for _, c := range st.cells {
					out = append(out, annotatedCell{
						phase:    ph.name,
						step:     st.name,
						action:   c.action.Name(),
						targetID: c.target.ID,
						kind:     cellStatusPhaseSkipped,
						seq:      seq,
					})
					seq++
				}
			}
			continue
		}
		for _, st := range ph.steps {
			for _, c := range st.cells {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				kind, detail := probeCellForPlan(ctx, c, h.DryRun)
				out = append(out, annotatedCell{
					phase:    ph.name,
					step:     st.name,
					action:   c.action.Name(),
					targetID: c.target.ID,
					kind:     kind,
					detail:   detail,
					seq:      seq,
				})
				seq++
			}
		}
	}
	return out, nil
}

func probeCellForPlan(ctx context.Context, c cellRef, pipelineDryRun bool) (cellStatusKind, string) {
	ok, err := c.action.Applicable(ctx, c.target)
	if err != nil {
		return cellStatusApplicableErr, err.Error()
	}
	if !ok {
		return cellStatusNotApplicable, ""
	}
	blocked, err := c.action.Check(ctx, c.target)
	if err != nil {
		return cellStatusCheckErr, err.Error()
	}
	if blocked {
		return cellStatusBlocked, ""
	}
	if pipelineDryRun {
		return cellStatusWouldExecute, ""
	}
	return cellStatusWillExecute, ""
}
