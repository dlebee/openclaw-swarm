package scaffold

import (
	"context"
)

// ProbeObserver is invoked once per (phase, target, step) cell immediately
// before the probe (Applicable+Check) runs. It's the only way the operator
// can tell which cell is currently "in flight" — some probes make network /
// SSH / multipass calls that can block indefinitely, so per-cell visibility
// is the difference between "is this hung?" and "it's waiting on X".
//
// Index is 1-based and total is the number of cells that will be probed
// (skipped phases are NOT counted, so the counter reflects real work).
// Implementations must not block: the observer is called on the probe
// goroutine and any delay becomes part of the probe latency.
type ProbeObserver interface {
	OnProbeStart(index, total int, phase, targetID, step string)
}

// ProbeNoop is a silent probe observer for callers that don't want progress.
type ProbeNoop struct{}

// OnProbeStart implements ProbeObserver.
func (ProbeNoop) OnProbeStart(int, int, string, string, string) {}

// annotatePlanCellsWithProbe walks the compiled plan, runs Applicable then Check
// for each (target, step) pair, and builds rows for the prepared-plan tree.
// Execute and Verify are not called.
func annotatePlanCellsWithProbe(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints) ([]annotatedCell, error) {
	return annotatePlanCellsWithProbeObs(ctx, compiled, h, ProbeNoop{})
}

// annotatePlanCellsWithProbeObs is the observer-aware variant. The plain
// annotatePlanCellsWithProbe delegates here with a no-op observer so legacy
// callers (describe_plain, plan_preview) keep their original signature.
func annotatePlanCellsWithProbeObs(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints, obs ProbeObserver) ([]annotatedCell, error) {
	if obs == nil {
		obs = ProbeNoop{}
	}
	total := countProbeCells(compiled, h)

	var out []annotatedCell
	seq := 0
	probed := 0
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
				probed++
				obs.OnProbeStart(probed, total, ph.name, t.ID, s.Name())
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

// countProbeCells returns how many (target, step) cells will actually hit
// Applicable+Check — phase-filtered cells are excluded so the progress
// denominator matches real work.
func countProbeCells(compiled []compiledPhase, h PlanDisplayHints) int {
	n := 0
	for _, ph := range compiled {
		if phaseFiltered(ph.name, h.OnlyPhases, h.SkipPhases) {
			continue
		}
		n += len(ph.targets) * len(ph.steps)
	}
	return n
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
