package scaffold

import "github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"

// ExecuteOptions controls execution.
//
// Phase filters (both names compare case-sensitively against Phase.Name):
//
//   - OnlyPhases, when non-empty, restricts execution to phases whose name is
//     in the list; every other phase is skipped (both probe and execute).
//     Useful for "just run provisioning" during integration tests.
//   - SkipPhases subtracts phases from the run. Applied AFTER OnlyPhases, so
//     e.g. OnlyPhases=[provisioning,security] + SkipPhases=[security] runs
//     only provisioning.
//
// Passing an unknown phase name is a no-op at the scaffold layer — callers
// (e.g. the CLI) should validate against Plan.PhaseNames before calling.
type ExecuteOptions struct {
	Progress   progress.Observer
	DryRun     bool
	OnlyPhases []string
	SkipPhases []string
}
