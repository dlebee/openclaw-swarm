package scaffold

// Phase is an ordered group of steps.
type Phase struct {
	Name        string
	Steps       []*Step
	Concurrency int
}

// AddStep registers a new step and returns it for chaining.
func (p *Phase) AddStep(name string) *Step {
	s := &Step{Name: name}
	p.Steps = append(p.Steps, s)
	return s
}
