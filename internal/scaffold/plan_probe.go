package scaffold

import (
	"context"
	"sync"
)

// ProbeObserver receives per-cell lifecycle events during the plan probe.
// It replaces the older index-based contract because the probe now runs
// targets within a phase in parallel (bounded by Phase.Concurrency), so a
// single "index/total" counter can't represent what's actually happening —
// multiple cells may be in flight at once.
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
//     "completed". Order of End events across cells is non-deterministic
//     under concurrency — use the (phase, targetID, step) tuple as the
//     key.
//   - Implementations are invoked from probe goroutines; they must be safe
//     for concurrent use and must not block (any delay becomes part of the
//     probe's wall-clock time).
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

// annotatePlanCellsWithProbeObs is the observer-aware, concurrent variant.
//
// Concurrency model (mirrors executor.executePhase):
//
//   - Phases run in declared order. Phase ordering is load-bearing because
//     some Check methods read plan-cache entries that earlier phases'
//     Checks populate (e.g. mesh-join Check reads CacheKeyControlURL /
//     CacheKeyPreauthKey written by mesh-gateway Check).
//   - Within a phase, targets probe in parallel up to ph.concurrency. Each
//     target has its own Payload, and any shared state flows through the
//     mutex-protected plan cache — so cross-target races are not possible
//     provided Check implementations use the plan-cache helpers.
//   - Within a target, steps probe sequentially. This preserves the
//     in-memory chain where, e.g., provisioning.CreateMachineStep.Check
//     populates MachineTarget.Instance and subsequent steps
//     (wait-cloud-init, ensure-agent-user, authorize-ssh-key) read it.
//     Interleaving those would produce phantom "not applicable" readings.
//
// Output ordering: cells are pre-allocated at their deterministic (phase,
// target, step) positions, and probe goroutines write into their own
// indices. No lock is needed on `out` because each goroutine owns a
// disjoint slice range.
func annotatePlanCellsWithProbeObs(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints, obs ProbeObserver) ([]annotatedCell, error) {
	if obs == nil {
		obs = ProbeNoop{}
	}

	// Pre-compute the cell layout. phaseStart[pi] is the index of the
	// first cell of phase pi in `out`. This lets each goroutine turn
	// (pi, ti, si) into an outIdx with plain arithmetic — no lookup table
	// or locking required.
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
					// Placeholder until the probe goroutine fills this in.
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
		if err := probePhase(ctx, ph, phaseStart[pi], out, obs, h.DryRun); err != nil {
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

// probePhase fans out targets across worker slots (bounded by ph.concurrency)
// and probes each target's steps sequentially. Writes results into `out` at
// the pre-assigned indices; no locking needed because each goroutine owns a
// disjoint range.
func probePhase(
	ctx context.Context,
	ph compiledPhase,
	phaseOffset int,
	out []annotatedCell,
	obs ProbeObserver,
	dryRun bool,
) error {
	if len(ph.targets) == 0 {
		return nil
	}

	// Separate cancel scope so a single ctx-cancel propagates to every
	// in-flight probe in this phase without bringing down the caller's ctx.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	concurrency := ph.concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	stepsInPhase := len(ph.steps)

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

			for si, s := range ph.steps {
				if ctx.Err() != nil {
					return
				}
				idx := phaseOffset + ti*stepsInPhase + si
				obs.OnProbeStart(ph.name, t.ID, s.Name())
				kind, detail := probeCellForPlan(ctx, t, s, dryRun)
				out[idx].kind = kind
				out[idx].detail = detail
				obs.OnProbeEnd(ph.name, t.ID, s.Name(), kind)
			}
		}(ti, t)
	}
	wg.Wait()
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
