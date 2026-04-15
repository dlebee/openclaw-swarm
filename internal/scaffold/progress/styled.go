package progress

import (
	"fmt"
	"io"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

var (
	stylePlanning = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	stylePhase    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleOk       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleSkip     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleStep     = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	styleCell     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

// Styled writes Lip Gloss execution and planning lines to w (thread-safe).
type Styled struct {
	mu sync.Mutex
	w  io.Writer
}

// NewStyled returns an Observer and BuildObserver writing to w.
func NewStyled(w io.Writer) *Styled {
	return &Styled{w: w}
}

func (s *Styled) println(a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintln(s.w, a...)
}

// OnPlanning implements BuildObserver.
func (s *Styled) OnPlanning(phase, step, action string) {
	line := stylePlanning.Render("Planning: ") + phase + " > " + step + " > " + action
	s.println(line)
}

// OnPhaseStart implements Observer.
func (s *Styled) OnPhaseStart(index, total int, phase string) {
	line := stylePhase.Render(fmt.Sprintf("Executing: [%d/%d] %s", index, total, phase))
	s.println(line)
}

// OnPhaseEnd implements Observer.
func (s *Styled) OnPhaseEnd(index, total int, phase string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		_, _ = fmt.Fprintln(s.w, styleErr.Render("  phase error: "), err)
	} else {
		_, _ = fmt.Fprintln(s.w, styleOk.Render("  phase done"))
	}
}

// OnStepStart implements Observer.
func (s *Styled) OnStepStart(phase, step string) {
	s.println(styleStep.Render("  step "), phase, "/", step)
}

// OnStepEnd implements Observer.
func (s *Styled) OnStepEnd(phase, step string, results []CellOutcome, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		_, _ = fmt.Fprintf(s.w, "%s %v\n", styleErr.Render("  step error:"), err)
		return
	}
	_, _ = fmt.Fprintf(s.w, "%s (%d cells)\n", styleOk.Render("  step ok"), len(results))
}

// OnCellStart implements Observer (no partial line; avoids interleaved output under concurrency).
func (s *Styled) OnCellStart(phase, step, targetID, actionName string) {}

// OnCellEnd implements Observer (one full line per cell, mutex-safe).
func (s *Styled) OnCellEnd(phase, step, targetID, actionName string, outcome CellOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := styleCell.Render("    ") + actionName + " @ " + targetID + ": "
	switch {
	case outcome.Err != nil:
		_, _ = fmt.Fprintln(s.w, prefix+styleErr.Render("error"), outcome.Err)
	case outcome.Skipped:
		_, _ = fmt.Fprintln(s.w, prefix+styleSkip.Render("skipped"))
	case outcome.Blocked:
		_, _ = fmt.Fprintln(s.w, prefix+styleSkip.Render("blocked"))
	default:
		_, _ = fmt.Fprintln(s.w, prefix+styleOk.Render("ok"))
	}
}
