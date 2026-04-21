package scaffold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

// ErrDeclined is returned when the user declines the confirm prompt.
var ErrDeclined = errors.New("scaffold: execution declined")

// PipelineOptions configures ExecWithConfirm.
type PipelineOptions struct {
	ExecuteOptions
	BuildProgress progress.BuildObserver
	Confirm       func() (bool, error)
	Width         int
	Out           io.Writer
	// PrettyPlan uses a Bubble Tea viewport (alternate screen) when Out is a TTY; otherwise DescribeStyledWithHints is printed.
	PrettyPlan bool
	// TeaProgressWriter, when non-nil, renders execution progress with a Bubble
	// Tea UI (worker bars + ✓/✗ completed) instead of the line-by-line observer.
	TeaProgressWriter io.Writer
}

var styleBuildBanner = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))

var styleConvergedBanner = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))

// ExecWithConfirm builds the plan, shows it, prompts, then executes.
// When DryRun is true, build + probed plan outline run (Applicable/Check per cell); no confirm, no Execute, no Verify.
//
// When the probe reports no remaining work (every cell is satisfied,
// not-applicable, or phase-skipped, and no probe errors), we skip the
// confirm prompt and the Execute pass entirely — there's nothing to run
// and nothing to verify (Verify only runs after a non-trivial Execute).
// This avoids a second round of Applicable+Check against already-converged
// infrastructure, which for mesh cells in particular means real SSH round
// trips per target. Force disables the short-circuit: a --force caller is
// explicitly asking Execute to run regardless of Check.
func ExecWithConfirm(ctx context.Context, p *Plan, o PipelineOptions) error {
	ctx = EnsurePlanCache(ctx)
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Width < 40 {
		o.Width = 80
	}
	_, _ = fmt.Fprintln(o.Out, styleBuildBanner.Render("Building plan…"))

	exec, err := buildPlanWithProgress(p, o.BuildProgress)
	if err != nil {
		return err
	}
	hints := PlanDisplayHints{
		DryRun:     o.DryRun,
		OnlyPhases: o.OnlyPhases,
		SkipPhases: o.SkipPhases,
		Force:      o.Force,
	}
	var summary ProbeSummary
	if o.PrettyPlan {
		if f, ok := o.Out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			// PlanPreview owns the probe internally (it needs the full
			// cell list for the viewport). We just hand it the plan.
			s, err := RunPlanPreview(ctx, exec, hints)
			if err != nil {
				return err
			}
			summary = s
		} else {
			treeOut, s, err := runPlanProbeWithSpinner(ctx, exec, o.Width, hints)
			if err != nil {
				return err
			}
			summary = s
			_, _ = fmt.Fprintln(o.Out)
			_, _ = fmt.Fprintln(o.Out, treeOut)
		}
	} else {
		treeOut, s, err := runPlanProbeWithSpinner(ctx, exec, o.Width, hints)
		if err != nil {
			return err
		}
		summary = s
		_, _ = fmt.Fprintln(o.Out)
		_, _ = fmt.Fprintln(o.Out, treeOut)
	}
	_, _ = fmt.Fprintln(o.Out)

	if o.DryRun {
		return nil
	}

	// Short-circuit when the probe shows no work. --force is treated as
	// "I want Execute to run"; the summary will naturally show WillExecute
	// cells so we never hit this branch under Force, but the explicit
	// guard documents the intent.
	if !o.Force && summary.NothingToDo() {
		_, _ = fmt.Fprintln(o.Out, styleConvergedBanner.Render("Nothing to apply — plan is fully converged."))
		return nil
	}

	confirm := o.Confirm
	if confirm == nil {
		confirm = func() (bool, error) { return true, nil }
	}
	ok, err := confirm()
	if err != nil {
		return err
	}
	if !ok {
		return ErrDeclined
	}

	if o.TeaProgressWriter != nil {
		execCtx, execCancel := context.WithCancel(ctx)
		defer execCancel()

		runner, obs := progress.NewExecTea(o.TeaProgressWriter, execCancel)
		go func() {
			opts := o.ExecuteOptions
			opts.Progress = obs
			execErr := exec.Execute(execCtx, opts)
			runner.Finish(execErr)
		}()
		return runner.Run()
	}

	opts := o.ExecuteOptions
	if opts.Progress == nil {
		opts.Progress = progress.Noop{}
	}
	return exec.Execute(ctx, opts)
}
