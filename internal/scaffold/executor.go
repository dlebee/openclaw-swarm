package scaffold

import (
	"context"
	"fmt"
	"sync"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

func runPlan(ctx context.Context, compiled []compiledPhase, opts ExecuteOptions) error {
	ctx = EnsurePlanCache(ctx)
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
		var phaseErr error
		for _, st := range ph.steps {
			obs.OnStepStart(ph.name, st.name)
			results, stepErr := executeStep(ctx, ph, st, obs, opts.DryRun)
			outcomes := resultsToOutcomes(results)
			obs.OnStepEnd(ph.name, st.name, outcomes, stepErr)
			if stepErr != nil {
				phaseErr = stepErr
				break
			}
			if st.barrier != nil {
				if err := st.barrier.Evaluate(ctx, st.name, results); err != nil {
					phaseErr = fmt.Errorf("barrier %q: %w", st.name, err)
					break
				}
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

func resultsToOutcomes(results []CellResult) []progress.CellOutcome {
	out := make([]progress.CellOutcome, len(results))
	for i := range results {
		out[i] = results[i].ToOutcome()
	}
	return out
}

func executeStep(ctx context.Context, ph compiledPhase, st compiledStep, obs progress.Observer, dryRun bool) ([]CellResult, error) {
	n := len(st.cells)
	if n == 0 {
		return nil, nil
	}
	results := make([]CellResult, n)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, ph.concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex

	for i, cell := range st.cells {
		wg.Add(1)
		go func(i int, c cellRef) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			obs.OnCellStart(ph.name, st.name, c.target.ID, c.action.Name())
			var res CellResult
			if dryRun {
				res = CellResult{TargetID: c.target.ID, ActionName: c.action.Name()}
			} else {
				res = runCell(ctx, c.target, c.action)
			}
			results[i] = res
			obs.OnCellEnd(ph.name, st.name, c.target.ID, c.action.Name(), res.ToOutcome())
			if res.Err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = res.Err
					cancel()
				}
				mu.Unlock()
			}
		}(i, cell)
	}
	wg.Wait()
	return results, firstErr
}
