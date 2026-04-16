package scaffold

import "github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"

type compiledPhase struct {
	name        string
	concurrency int
	targets     []Target
	steps       []Step
	barrier     Barrier
}

func compilePlan(p *Plan, obs []progress.BuildObserver) []compiledPhase {
	out := make([]compiledPhase, 0, len(p.Phases))
	for _, ph := range p.Phases {
		cp := compiledPhase{
			name:        ph.Name,
			concurrency: ph.Concurrency,
			targets:     ph.Targets,
			steps:       ph.Steps,
			barrier:     ph.Barrier,
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
	return out
}
