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
	Satisfied bool
	Err      error
}

// ToOutcome maps to progress.CellOutcome for observers.
func (r CellResult) ToOutcome() progress.CellOutcome {
	return progress.CellOutcome{
		Skipped: r.Skipped,
		Satisfied: r.Satisfied,
		Err:     r.Err,
	}
}

// runCell runs Applicable → Check → Execute → Verify for one (target, step) pair.
//
// When force is true, Check is skipped entirely — the cell proceeds from a
// successful Applicable directly to Execute. This is the "I know the remote
// has drifted in a way my check script can't detect, just run it" escape
// hatch (see ExecuteOptions.Force). Applicable is NOT bypassed: it's the
// structural gate ("does this step even target this payload type") rather
// than an idempotency probe, and skipping it could e.g. scp-upload to a
// target the step wasn't meant for. Verify still runs after Execute.
func runCell(ctx context.Context, t Target, s Step, force bool) CellResult {
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
	if !force {
		satisfied, err := s.Check(ctx, t)
		if err != nil {
			res.Err = err
			return res
		}
		if satisfied {
			res.Satisfied = true
			return res
		}
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
