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
	styleTarget   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleStep     = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
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
func (s *Styled) OnPlanning(phase, step string) {
	line := stylePlanning.Render("Planning: ") + phase + " > " + step
	s.println(line)
}

// OnPhaseStart implements Observer.
func (s *Styled) OnPhaseStart(index, total int, phase string) {
	line := stylePhase.Render(fmt.Sprintf("Phase [%d/%d] %s", index, total, phase))
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

// OnTargetStart implements Observer.
func (s *Styled) OnTargetStart(phase, targetID string, slot int) {
	line := fmt.Sprintf("  [%d] ", slot) + styleTarget.Render(targetID) + " starting"
	s.println(line)
}

// OnTargetEnd implements Observer.
func (s *Styled) OnTargetEnd(phase, targetID string, slot int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := fmt.Sprintf("  [%d] %s: ", slot, targetID)
	if err != nil {
		_, _ = fmt.Fprintln(s.w, prefix+styleErr.Render("error ")+fmt.Sprint(err))
	} else {
		_, _ = fmt.Fprintln(s.w, prefix+styleOk.Render("done"))
	}
}

// OnStepStart implements Observer.
func (s *Styled) OnStepStart(phase, targetID, step string, slot int) {
	line := fmt.Sprintf("  [%d] %s > ", slot, targetID) + styleStep.Render(step)
	s.println(line)
}

// OnStepEnd implements Observer.
func (s *Styled) OnStepEnd(phase, targetID, step string, slot int, outcome CellOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := fmt.Sprintf("  [%d] %s > %s: ", slot, targetID, step)
	switch {
	case outcome.Err != nil:
		_, _ = fmt.Fprintln(s.w, prefix+styleErr.Render("error ")+fmt.Sprint(outcome.Err))
	case outcome.Skipped:
		_, _ = fmt.Fprintln(s.w, prefix+styleSkip.Render("skipped"))
	case outcome.Satisfied:
		_, _ = fmt.Fprintln(s.w, prefix+styleSkip.Render("satisfied"))
	default:
		_, _ = fmt.Fprintln(s.w, prefix+styleOk.Render("ok"))
	}
}
