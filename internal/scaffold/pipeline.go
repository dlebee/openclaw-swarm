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

// ExecWithConfirm builds the plan, shows it, prompts, then executes.
// When DryRun is true, build + probed plan outline run (Applicable/Check per cell); no confirm, no Execute, no Verify.
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
	if o.PrettyPlan {
		if f, ok := o.Out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			// PlanPreview owns the probe internally (it needs the full
			// cell list for the viewport). We just hand it the plan.
			if err := RunPlanPreview(ctx, exec, hints); err != nil {
				return err
			}
		} else {
			treeOut, err := runPlanProbeWithSpinner(ctx, exec, o.Width, hints)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(o.Out)
			_, _ = fmt.Fprintln(o.Out, treeOut)
		}
	} else {
		treeOut, err := runPlanProbeWithSpinner(ctx, exec, o.Width, hints)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(o.Out)
		_, _ = fmt.Fprintln(o.Out, treeOut)
	}
	_, _ = fmt.Fprintln(o.Out)

	if o.DryRun {
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
