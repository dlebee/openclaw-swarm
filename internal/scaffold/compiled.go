package scaffold

import (
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

type compiledPhase struct {
	name             string
	concurrency      int
	probeConcurrency int
	targets          []Target
	steps            []Step
	barrier          Barrier
	probeDependsOn   []string // resolved at compile time (see compilePlan)
}

func compilePlan(p *Plan, obs []progress.BuildObserver) ([]compiledPhase, error) {
	nameIdx := make(map[string]int, len(p.Phases))
	for i, ph := range p.Phases {
		if _, dup := nameIdx[ph.Name]; dup {
			return nil, fmt.Errorf("scaffold: duplicate phase name %q", ph.Name)
		}
		nameIdx[ph.Name] = i
	}

	out := make([]compiledPhase, 0, len(p.Phases))
	for i, ph := range p.Phases {
		var deps []string
		if ph.ProbeDependsOn == nil {
			if i > 0 {
				deps = []string{p.Phases[i-1].Name}
			}
		} else {
			deps = append([]string(nil), ph.ProbeDependsOn...)
		}
		for _, d := range deps {
			if d == ph.Name {
				return nil, fmt.Errorf("scaffold: phase %q cannot probe-depend on itself", ph.Name)
			}
			if _, ok := nameIdx[d]; !ok {
				return nil, fmt.Errorf("scaffold: phase %q probe-depends on unknown phase %q", ph.Name, d)
			}
		}

		probeConc := ph.ProbeConcurrency
		if probeConc == 0 {
			probeConc = 1
		}

		cp := compiledPhase{
			name:             ph.Name,
			concurrency:      ph.Concurrency,
			probeConcurrency: probeConc,
			targets:          ph.Targets,
			steps:            ph.Steps,
			barrier:          ph.Barrier,
			probeDependsOn:   deps,
		}
		for _, s := range ph.Steps {
			for _, o := range obs {
				if o != nil {
					o.OnPlanning(ph.Name, s.Name())
				}
			}
		}
		out = append(out, cp)
	}
	return out, nil
}
