package scaffold

import "context"

// ProbeObserver receives per-cell lifecycle events during the plan probe.
//
// Contract:
//   - Total is passed separately (see ProbeCellCount) before the probe
//     starts. It counts only cells that will actually be probed
//     (phase-filtered cells are excluded).
//   - OnProbeStart fires BEFORE Applicable+Check runs for (phase, target,
//     step). This is the load-bearing part: if the probe hangs, the last
//     OnProbeStart events identify exactly which cells are wedged.
//   - OnProbeEnd fires AFTER the probe finishes, carrying the resulting
//     cell status so a UI can move the cell out of "in flight" and into
//     "completed". Probing runs one cell at a time within a phase (targets
//     and steps in plan order), so at most one (phase, target, step) is
//     in flight unless the observer is shared with other work.
//   - Implementations should not block for long; any delay becomes part of
//     the probe's wall-clock time.
type ProbeObserver interface {
	OnProbeStart(phase, targetID, step string)
	OnProbeEnd(phase, targetID, step string, kind cellStatusKind)
}

// ProbeNoop is a silent probe observer for callers that don't want progress.
type ProbeNoop struct{}

// OnProbeStart implements ProbeObserver.
func (ProbeNoop) OnProbeStart(string, string, string) {}

// OnProbeEnd implements ProbeObserver.
func (ProbeNoop) OnProbeEnd(string, string, string, cellStatusKind) {}

// annotatePlanCellsWithProbe walks the compiled plan, runs Applicable then Check
// for each (target, step) pair, and builds rows for the prepared-plan tree.
// Execute and Verify are not called.
func annotatePlanCellsWithProbe(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints) ([]annotatedCell, error) {
	return annotatePlanCellsWithProbeObs(ctx, compiled, h, ProbeNoop{})
}

// annotatePlanCellsWithProbeObs is the observer-aware variant.
//
// Concurrency model:
//
//   - Phases run in declared order. Check methods in later phases may rely
//     on plan-cache entries populated by earlier phases' Checks (control URL,
//     preauth key, etc.), so we do not probe phases concurrently.
//   - Within a phase, probing is strictly sequential: targets in plan order,
//     and for each target, steps in plan order. Parallel probing per target
//     (or across targets) is unsafe because earlier Checks hydrate shared
//     target payloads (e.g. provisioning create-machine before authorize-
//     ssh-key). Phase.Concurrency applies to Execute, not to probe.
//
// Output ordering: cells are pre-allocated at deterministic indices; the
// sequential probe writes each cell in turn.
func annotatePlanCellsWithProbeObs(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints, obs ProbeObserver) ([]annotatedCell, error) {
	if obs == nil {
		obs = ProbeNoop{}
	}

	// Pre-compute the cell layout. phaseStart[pi] is the index of the
	// first cell of phase pi in `out` (flat index: ti*stepsInPhase+si).
	phaseStart := make([]int, len(compiled))
	total := 0
	for pi, ph := range compiled {
		phaseStart[pi] = total
		total += len(ph.targets) * len(ph.steps)
	}

	out := make([]annotatedCell, total)
	for pi, ph := range compiled {
		filtered := phaseFiltered(ph.name, h.OnlyPhases, h.SkipPhases)
		for ti, t := range ph.targets {
			for si, s := range ph.steps {
				idx := cellOutIdx(phaseStart, pi, len(ph.steps), ti, si)
				kind := cellStatusPhaseSkipped
				if !filtered {
					// Placeholder until the probe run fills this in.
					// We never leave these placeholder values in a returned
					// slice unless ctx was cancelled mid-probe (in which case
					// we return the error instead).
					kind = cellStatusNotApplicable
				}
				out[idx] = annotatedCell{
					phase:    ph.name,
					step:     s.Name(),
					targetID: t.ID,
					kind:     kind,
					seq:      idx,
				}
			}
		}
	}

	// Phase-by-phase parallel probe. We can't fan out across phases because
	// Check methods sometimes rely on plan-cache entries populated by
	// earlier phases' Checks (control URL, preauth key, etc.).
	for pi, ph := range compiled {
		if phaseFiltered(ph.name, h.OnlyPhases, h.SkipPhases) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := probePhase(ctx, ph, phaseStart[pi], out, obs, h.DryRun, h.Force); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// cellOutIdx maps (phase index, target index, step index) to the position of
// that cell in the flattened output slice. Layout is:
//
//	phaseStart[pi] + ti * stepsInPhase + si
//
// which matches the nested iteration order used when pre-allocating `out`.
func cellOutIdx(phaseStart []int, pi, stepsInPhase, ti, si int) int {
	return phaseStart[pi] + ti*stepsInPhase + si
}

// probePhase runs Applicable+Check for every cell in deterministic order:
// targets in plan order, steps in plan order for each target. Fully
// sequential so later steps never race earlier hydration on shared payloads.
func probePhase(
	ctx context.Context,
	ph compiledPhase,
	phaseOffset int,
	out []annotatedCell,
	obs ProbeObserver,
	dryRun, force bool,
) error {
	if len(ph.targets) == 0 || len(ph.steps) == 0 {
		return nil
	}

	stepsInPhase := len(ph.steps)
	for ti, t := range ph.targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		for si, s := range ph.steps {
			if err := ctx.Err(); err != nil {
				return err
			}
			idx := phaseOffset + ti*stepsInPhase + si
			obs.OnProbeStart(ph.name, t.ID, s.Name())
			kind, detail := probeCellForPlan(ctx, t, s, dryRun, force)
			out[idx].kind = kind
			out[idx].detail = detail
			obs.OnProbeEnd(ph.name, t.ID, s.Name(), kind)
		}
	}
	return ctx.Err()
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

func probeCellForPlan(ctx context.Context, t Target, s Step, pipelineDryRun, force bool) (cellStatusKind, string) {
	ok, err := s.Applicable(ctx, t)
	if err != nil {
		return cellStatusApplicableErr, err.Error()
	}
	if !ok {
		return cellStatusNotApplicable, ""
	}
	if !force {
		satisfied, err := s.Check(ctx, t)
		if err != nil {
			return cellStatusCheckErr, err.Error()
		}
		if satisfied {
			return cellStatusSatisfied, ""
		}
	}
	if pipelineDryRun {
		return cellStatusWouldExecute, ""
	}
	return cellStatusWillExecute, ""
}
