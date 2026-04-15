package scaffold

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
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
}

var styleBuildBanner = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))

// ExecWithConfirm builds the plan, shows it, prompts, then executes.
func ExecWithConfirm(ctx context.Context, p *Plan, o PipelineOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Width < 40 {
		o.Width = 80
	}
	_, _ = fmt.Fprintln(o.Out, styleBuildBanner.Render("Building plan…"))

	exec, err := p.Build(o.BuildProgress)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(o.Out)
	_, _ = fmt.Fprintln(o.Out, exec.DescribeStyled(o.Width))
	_, _ = fmt.Fprintln(o.Out)

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
