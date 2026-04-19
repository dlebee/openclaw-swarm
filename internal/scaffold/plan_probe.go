package scaffold

import (
	"context"
)

// annotatePlanCellsWithProbe walks the compiled plan, runs Applicable then Check
// for each (target, step) pair, and builds rows for the prepared-plan tree.
// Execute and Verify are not called.
func annotatePlanCellsWithProbe(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints) ([]annotatedCell, error) {
	var out []annotatedCell
	seq := 0
	for _, ph := range compiled {
		if phaseFiltered(ph.name, h.OnlyPhases, h.SkipPhases) {
			for _, t := range ph.targets {
				for _, s := range ph.steps {
					out = append(out, annotatedCell{
						phase:    ph.name,
						step:     s.Name(),
						targetID: t.ID,
						kind:     cellStatusPhaseSkipped,
						seq:      seq,
					})
					seq++
				}
			}
			continue
		}
		for _, t := range ph.targets {
			for _, s := range ph.steps {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				kind, detail := probeCellForPlan(ctx, t, s, h.DryRun)
				out = append(out, annotatedCell{
					phase:    ph.name,
					step:     s.Name(),
					targetID: t.ID,
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

func probeCellForPlan(ctx context.Context, t Target, s Step, pipelineDryRun bool) (cellStatusKind, string) {
	ok, err := s.Applicable(ctx, t)
	if err != nil {
		return cellStatusApplicableErr, err.Error()
	}
	if !ok {
		return cellStatusNotApplicable, ""
	}
	satisfied, err := s.Check(ctx, t)
	if err != nil {
		return cellStatusCheckErr, err.Error()
	}
	if satisfied {
		return cellStatusSatisfied, ""
	}
	if pipelineDryRun {
		return cellStatusWouldExecute, ""
	}
	return cellStatusWillExecute, ""
}
