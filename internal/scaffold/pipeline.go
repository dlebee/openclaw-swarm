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
	_, _ = fmt.Fprintln(o.Out)
	hints := PlanDisplayHints{DryRun: o.DryRun, SkipPhases: o.SkipPhases}
	if o.PrettyPlan {
		if f, ok := o.Out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			if err := RunPlanPreview(ctx, exec, hints); err != nil {
				return err
			}
		} else {
			treeOut, err := exec.DescribeStyledWithHints(ctx, o.Width, hints)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(o.Out, treeOut)
		}
	} else {
		treeOut, err := exec.DescribeStyledWithHints(ctx, o.Width, hints)
		if err != nil {
			return err
		}
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

	opts := o.ExecuteOptions
	if opts.Progress == nil {
		opts.Progress = progress.Noop{}
	}
	return exec.Execute(ctx, opts)
}
