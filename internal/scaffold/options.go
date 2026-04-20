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

	// Force, when true, skips the Check predicate for every applicable
	// (target, step) cell and goes straight to Execute. Applicable is
	// still consulted — "this step doesn't target this payload type" is
	// a structural gate, not an idempotency check, and bypassing it
	// would try to e.g. scp.upload to a machine the step wasn't meant
	// for. Verify still runs after Execute.
	//
	// Intended for "I know the remote drifted in a way my check script
	// can't detect; just run the damn step." Most obviously useful for
	// automations (one-shot maintenance scripts) but safe to set on any
	// plan.
	Force bool
}
