package scaffold

import "context"

// Step is one unit of work in a phase. For each target in the phase,
// the step's lifecycle (Applicable → Check → Execute → Verify) runs in order.
// Steps within a target pipeline run sequentially; targets run concurrently.
type Step interface {
	Name() string
	Applicable(ctx context.Context, t Target) (bool, error)
	Check(ctx context.Context, t Target) (blocked bool, err error)
	Execute(ctx context.Context, t Target) error
	Verify(ctx context.Context, t Target) error
}
