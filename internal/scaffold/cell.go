package scaffold

import (
	"context"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// CellResult is the outcome of running one (target, step) pair.
type CellResult struct {
	TargetID string
	StepName string
	Skipped  bool
	Blocked  bool
	Err      error
}

// ToOutcome maps to progress.CellOutcome for observers.
func (r CellResult) ToOutcome() progress.CellOutcome {
	return progress.CellOutcome{
		Skipped: r.Skipped,
		Blocked: r.Blocked,
		Err:     r.Err,
	}
}

// runCell runs Applicable → Check → Execute → Verify for one (target, step) pair.
func runCell(ctx context.Context, t Target, s Step) CellResult {
	res := CellResult{TargetID: t.ID, StepName: s.Name()}
	ok, err := s.Applicable(ctx, t)
	if err != nil {
		res.Err = err
		return res
	}
	if !ok {
		res.Skipped = true
		return res
	}
	blocked, err := s.Check(ctx, t)
	if err != nil {
		res.Err = err
		return res
	}
	if blocked {
		res.Blocked = true
		return res
	}
	if err := s.Execute(ctx, t); err != nil {
		res.Err = err
		return res
	}
	if err := s.Verify(ctx, t); err != nil {
		res.Err = err
		return res
	}
	return res
}
