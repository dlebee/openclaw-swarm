package scaffold

import "github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"

type compiledPhase struct {
	name        string
	concurrency int
	steps       []compiledStep
}

type compiledStep struct {
	name    string
	cells   []cellRef
	barrier Barrier
}

type cellRef struct {
	target Target
	action Action
}

func compilePlan(p *Plan, obs []progress.BuildObserver) []compiledPhase {
	out := make([]compiledPhase, 0, len(p.Phases))
	for _, ph := range p.Phases {
		cp := compiledPhase{name: ph.Name, concurrency: ph.Concurrency}
		for _, st := range ph.Steps {
			cs := compiledStep{name: st.Name, barrier: st.Barrier}
			for _, t := range st.Targets {
				for _, a := range st.Actions {
					for _, o := range obs {
						if o != nil {
							o.OnPlanning(ph.Name, st.Name, a.Name())
						}
					}
					cs.cells = append(cs.cells, cellRef{target: t, action: a})
				}
			}
			cp.steps = append(cp.steps, cs)
		}
		out = append(out, cp)
	}
	return out
}
