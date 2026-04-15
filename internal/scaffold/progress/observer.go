package progress

// CellOutcome is the result of one cell execution (no scaffold import).
type CellOutcome struct {
	Skipped bool
	Blocked bool
	Err     error
}

// Observer receives execution lifecycle events.
type Observer interface {
	OnPhaseStart(index, total int, phase string)
	OnPhaseEnd(index, total int, phase string, err error)
	OnStepStart(phase, step string)
	OnStepEnd(phase, step string, results []CellOutcome, err error)
	OnCellStart(phase, step, targetID, actionName string)
	OnCellEnd(phase, step, targetID, actionName string, outcome CellOutcome)
}

// BuildObserver receives planning / compile-time progress.
type BuildObserver interface {
	OnPlanning(phase, step, action string)
}

// Noop is a silent execution observer.
type Noop struct{}

func (Noop) OnPhaseStart(int, int, string)                          {}
func (Noop) OnPhaseEnd(int, int, string, error)                      {}
func (Noop) OnStepStart(string, string)                             {}
func (Noop) OnStepEnd(string, string, []CellOutcome, error)         {}
func (Noop) OnCellStart(string, string, string, string)            {}
func (Noop) OnCellEnd(string, string, string, string, CellOutcome) {}

// NoopBuild is a silent planning observer.
type NoopBuild struct{}

func (NoopBuild) OnPlanning(string, string, string) {}
