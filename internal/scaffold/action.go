package scaffold

import "context"

// Action is one column in the step matrix. Each (target, action) pair is a cell.
type Action interface {
	Name() string
	// Applicable returns false to skip the cell (conditions).
	Applicable(ctx context.Context, t Target) (bool, error)
	// Check returns blocked=true to skip Execute and Verify.
	Check(ctx context.Context, t Target) (blocked bool, err error)
	Execute(ctx context.Context, t Target) error
	Verify(ctx context.Context, t Target) error
}
