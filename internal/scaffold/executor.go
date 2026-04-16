package scaffold

import (
	"context"
	"fmt"
	"sync"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

func runPlan(ctx context.Context, compiled []compiledPhase, opts ExecuteOptions) error {
	ctx = EnsurePlanCache(ctx)
	defer ClosePlanResources(ctx)
	obs := opts.Progress
	if obs == nil {
		obs = progress.Noop{}
	}
	total := len(compiled)
	for pi, ph := range compiled {
		if skipName(ph.name, opts.SkipPhases) {
			continue
		}
		obs.OnPhaseStart(pi+1, total, ph.name)
		results, phaseErr := executePhase(ctx, ph, obs, opts.DryRun)
		if phaseErr == nil && ph.barrier != nil {
			if err := ph.barrier.Evaluate(ctx, ph.name, results); err != nil {
				phaseErr = fmt.Errorf("barrier %q: %w", ph.name, err)
			}
		}
		obs.OnPhaseEnd(pi+1, total, ph.name, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
	}
	return nil
}

func skipName(name string, skips []string) bool {
	for _, s := range skips {
		if s == name {
			return true
		}
	}
	return false
}

// executePhase fans out targets across worker slots, each running its step
// pipeline sequentially. Returns all cell results and the first target error.
func executePhase(ctx context.Context, ph compiledPhase, obs progress.Observer, dryRun bool) ([]CellResult, error) {
	nTargets := len(ph.targets)
	if nTargets == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	slots := make(chan int, ph.concurrency)
	for i := 0; i < ph.concurrency; i++ {
		slots <- i
	}

	type targetResult struct {
		cells []CellResult
		err   error
	}

	results := make([]targetResult, nTargets)
	var wg sync.WaitGroup

	for ti, target := range ph.targets {
		wg.Add(1)
		go func(ti int, t Target) {
			defer wg.Done()

			var slot int
			select {
			case slot = <-slots:
				defer func() { slots <- slot }()
			case <-ctx.Done():
				return
			}

			obs.OnTargetStart(ph.name, t.ID, slot)
			var cells []CellResult
			var targetErr error

			for _, step := range ph.steps {
				if ctx.Err() != nil {
					targetErr = ctx.Err()
					break
				}
				obs.OnStepStart(ph.name, t.ID, step.Name(), slot)
				var res CellResult
				if dryRun {
					res = CellResult{TargetID: t.ID, StepName: step.Name()}
				} else {
					res = runCell(ctx, t, step)
				}
				cells = append(cells, res)
				obs.OnStepEnd(ph.name, t.ID, step.Name(), slot, res.ToOutcome())
				if res.Err != nil {
					targetErr = res.Err
					break
				}
			}

			obs.OnTargetEnd(ph.name, t.ID, slot, targetErr)
			results[ti] = targetResult{cells: cells, err: targetErr}
		}(ti, target)
	}

	wg.Wait()

	var allCells []CellResult
	var firstErr error
	for _, tr := range results {
		allCells = append(allCells, tr.cells...)
		if tr.err != nil && firstErr == nil {
			firstErr = tr.err
		}
	}
	return allCells, firstErr
}
