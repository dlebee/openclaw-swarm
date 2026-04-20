package scaffold

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// computeProbeWaves partitions active (non-filtered) phase indices into waves
// for parallel probing. An edge dep → ph means phase ph's probe waits until
// phase dep's probe has finished. Dependencies on filtered-out phases are
// ignored.
func computeProbeWaves(compiled []compiledPhase, h PlanDisplayHints) ([][]int, error) {
	nameToIdx := make(map[string]int, len(compiled))
	for i, ph := range compiled {
		nameToIdx[ph.name] = i
	}

	active := make([]int, 0, len(compiled))
	for pi, ph := range compiled {
		if phaseFiltered(ph.name, h.OnlyPhases, h.SkipPhases) {
			continue
		}
		active = append(active, pi)
	}
	activeSet := make(map[int]struct{}, len(active))
	for _, pi := range active {
		activeSet[pi] = struct{}{}
	}

	indegree := make(map[int]int, len(active))
	succ := make(map[int][]int)
	for _, pi := range active {
		indegree[pi] = 0
	}
	for _, pi := range active {
		for _, dname := range compiled[pi].probeDependsOn {
			if phaseFiltered(dname, h.OnlyPhases, h.SkipPhases) {
				continue
			}
			j, ok := nameToIdx[dname]
			if !ok {
				continue
			}
			if _, ok := activeSet[j]; !ok {
				continue
			}
			indegree[pi]++
			succ[j] = append(succ[j], pi)
		}
	}

	var q []int
	for _, pi := range active {
		if indegree[pi] == 0 {
			q = append(q, pi)
		}
	}

	var waves [][]int
	processed := 0
	for len(q) > 0 {
		wave := append([]int(nil), q...)
		sort.Ints(wave)
		waves = append(waves, wave)
		processed += len(wave)
		q = q[:0]
		for _, v := range wave {
			for _, w := range succ[v] {
				indegree[w]--
				if indegree[w] == 0 {
					q = append(q, w)
				}
			}
		}
	}
	if processed != len(active) {
		return nil, fmt.Errorf("scaffold: probe dependency cycle among phases (check Phase.ProbeDependsOn)")
	}
	return waves, nil
}

func runProbeWaves(
	ctx context.Context,
	waves [][]int,
	compiled []compiledPhase,
	phaseStart []int,
	out []annotatedCell,
	obs ProbeObserver,
	dryRun, force bool,
) error {
	for _, wave := range waves {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(wave) == 1 {
			pi := wave[0]
			if err := probePhase(ctx, compiled[pi], phaseStart[pi], out, obs, dryRun, force); err != nil {
				return err
			}
			continue
		}

		waveCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		for _, pi := range wave {
			wg.Add(1)
			go func(pi int) {
				defer wg.Done()
				err := probePhase(waveCtx, compiled[pi], phaseStart[pi], out, obs, dryRun, force)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
				}
			}(pi)
		}
		wg.Wait()
		cancel()
		if err := firstErr; err != nil {
			return err
		}
	}
	return nil
}
