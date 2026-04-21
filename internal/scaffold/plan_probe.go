package scaffold

import (
	"context"
	"sync"
	"time"
)

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
//     "completed". Within a phase, steps for one target run in order; multiple
//     targets may probe at once when Phase.ProbeConcurrency > 1. Independent
//     phases (per Phase.ProbeDependsOn) may also probe concurrently, so
//     observers may see multiple cells in flight.
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

// ProbeSummary aggregates cell verdicts from a completed probe. Callers use
// it to decide whether there is any real work for the Execute pass — a
// fully-converged plan (no will-execute cells and no probe errors) should
// short-circuit confirm + Execute so users don't re-probe an already-
// converged system.
type ProbeSummary struct {
	PhaseSkipped  int
	NotApplicable int
	Satisfied     int
	WillExecute   int
	WouldExecute  int
	ApplicableErr int
	CheckErr      int
}

// NothingToDo reports whether the probe verdicts show no remaining work
// AND no errors. When true, ExecWithConfirm skips the confirm prompt and
// does not call Execute: there is nothing to run and nothing to verify
// (Verify only runs after a non-trivial Execute). Any probe error disables
// the short-circuit so the user sees and decides on the failure.
//
// WouldExecute is ignored here because it only appears under DryRun, and
// DryRun returns from ExecWithConfirm before the short-circuit is
// evaluated.
func (s ProbeSummary) NothingToDo() bool {
	return s.WillExecute == 0 && s.ApplicableErr == 0 && s.CheckErr == 0
}

// summariseCells tallies the terminal cell statuses from an annotated
// probe result.
func summariseCells(cells []annotatedCell) ProbeSummary {
	var s ProbeSummary
	for _, c := range cells {
		switch c.kind {
		case cellStatusPhaseSkipped:
			s.PhaseSkipped++
		case cellStatusNotApplicable:
			s.NotApplicable++
		case cellStatusSatisfied:
			s.Satisfied++
		case cellStatusWillExecute:
			s.WillExecute++
		case cellStatusWouldExecute:
			s.WouldExecute++
		case cellStatusApplicableErr:
			s.ApplicableErr++
		case cellStatusCheckErr:
			s.CheckErr++
		}
	}
	return s
}

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
//   - Between phases, probing follows Phase.ProbeDependsOn (resolved at
//     Build): phases in the same wave have no active mutual dependencies and
//     may run Applicable+Check in parallel. Execute still runs phases in
//     strict plan append order.
//   - Within a phase, steps for one target run in plan order. Across targets,
//     up to Phase.ProbeConcurrency probes may run in parallel (default 1).
//     Phase.Concurrency applies only to Execute.
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

	waves, err := computeProbeWaves(compiled, h)
	if err != nil {
		return nil, err
	}
	if err := runProbeWaves(ctx, waves, compiled, phaseStart, out, obs, h.DryRun, h.Force); err != nil {
		return nil, err
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

// probePhase runs Applicable+Check for every cell. Steps for a target are
// sequential; targets may probe in parallel up to ph.probeConcurrency.
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
	n := ph.probeConcurrency
	if n < 1 {
		n = 1
	}
	if n == 1 {
		for ti, t := range ph.targets {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := probeTargetRow(ctx, ph, phaseOffset, ti, t, stepsInPhase, out, obs, dryRun, force); err != nil {
				return err
			}
		}
		return ctx.Err()
	}

	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for ti, t := range ph.targets {
		wg.Add(1)
		go func(ti int, t Target) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			_ = probeTargetRow(ctx, ph, phaseOffset, ti, t, stepsInPhase, out, obs, dryRun, force)
		}(ti, t)
	}
	wg.Wait()
	return ctx.Err()
}

func probeTargetRow(
	ctx context.Context,
	ph compiledPhase,
	phaseOffset, ti int,
	t Target,
	stepsInPhase int,
	out []annotatedCell,
	obs ProbeObserver,
	dryRun, force bool,
) error {
	for si, s := range ph.steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		idx := phaseOffset + ti*stepsInPhase + si
		obs.OnProbeStart(ph.name, t.ID, s.Name())
		kind, detail, app, chk, checkRan := probeCellForPlan(ctx, t, s, dryRun, force)
		out[idx].kind = kind
		out[idx].detail = detail
		out[idx].applicableDur = app
		out[idx].checkDur = chk
		out[idx].checkRan = checkRan
		obs.OnProbeEnd(ph.name, t.ID, s.Name(), kind)
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

func probeCellForPlan(ctx context.Context, t Target, s Step, pipelineDryRun, force bool) (kind cellStatusKind, detail string, app, chk time.Duration, checkRan bool) {
	t0 := time.Now()
	ok, err := s.Applicable(ctx, t)
	app = time.Since(t0)
	if err != nil {
		return cellStatusApplicableErr, err.Error(), app, 0, false
	}
	if !ok {
		return cellStatusNotApplicable, "", app, 0, false
	}
	if !force {
		t1 := time.Now()
		satisfied, err := s.Check(ctx, t)
		chk = time.Since(t1)
		checkRan = true
		if err != nil {
			return cellStatusCheckErr, err.Error(), app, chk, checkRan
		}
		if satisfied {
			return cellStatusSatisfied, "", app, chk, checkRan
		}
	}
	if pipelineDryRun {
		return cellStatusWouldExecute, "", app, chk, checkRan
	}
	return cellStatusWillExecute, "", app, chk, checkRan
}
