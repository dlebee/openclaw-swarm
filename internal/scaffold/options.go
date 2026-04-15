package scaffold

import "github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"

// ExecuteOptions controls execution.
type ExecuteOptions struct {
	Progress   progress.Observer
	DryRun     bool
	SkipPhases []string
}
