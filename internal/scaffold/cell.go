package scaffold

import (
	"context"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// CellResult is the outcome of running one cell.
type CellResult struct {
	TargetID   string
	ActionName string
	Skipped    bool
	Blocked    bool
	Err        error
}

// ToOutcome maps to progress.CellOutcome for observers.
func (r CellResult) ToOutcome() progress.CellOutcome {
	return progress.CellOutcome{
		Skipped: r.Skipped,
		Blocked: r.Blocked,
		Err:     r.Err,
	}
}

// runCell runs Applicable > Check > Execute > Verify.
func runCell(ctx context.Context, t Target, a Action) CellResult {
	res := CellResult{TargetID: t.ID, ActionName: a.Name()}
	ok, err := a.Applicable(ctx, t)
	if err != nil {
		res.Err = err
		return res
	}
	if !ok {
		res.Skipped = true
		return res
	}
	blocked, err := a.Check(ctx, t)
	if err != nil {
		res.Err = err
		return res
	}
	if blocked {
		res.Blocked = true
		return res
	}
	if err := a.Execute(ctx, t); err != nil {
		res.Err = err
		return res
	}
	if err := a.Verify(ctx, t); err != nil {
		res.Err = err
		return res
	}
	return res
}
