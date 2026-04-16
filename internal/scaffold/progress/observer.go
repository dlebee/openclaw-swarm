package progress

// CellOutcome is the result of one (target, step) execution (no scaffold import).
type CellOutcome struct {
	Skipped bool
	Satisfied bool
	Err     error
}

// Observer receives execution lifecycle events.
// Slot is the worker index (0..concurrency-1) assigned to the target.
type Observer interface {
	OnPhaseStart(index, total int, phase string)
	OnPhaseEnd(index, total int, phase string, err error)
	OnTargetStart(phase, targetID string, slot int)
	OnTargetEnd(phase, targetID string, slot int, err error)
	OnStepStart(phase, targetID, step string, slot int)
	OnStepEnd(phase, targetID, step string, slot int, outcome CellOutcome)
}

// BuildObserver receives planning / compile-time progress.
type BuildObserver interface {
	OnPlanning(phase, step string)
}

// Noop is a silent execution observer.
type Noop struct{}

func (Noop) OnPhaseStart(int, int, string)                      {}
func (Noop) OnPhaseEnd(int, int, string, error)                  {}
func (Noop) OnTargetStart(string, string, int)                   {}
func (Noop) OnTargetEnd(string, string, int, error)              {}
func (Noop) OnStepStart(string, string, string, int)             {}
func (Noop) OnStepEnd(string, string, string, int, CellOutcome)  {}

// NoopBuild is a silent planning observer.
type NoopBuild struct{}

func (NoopBuild) OnPlanning(string, string) {}
