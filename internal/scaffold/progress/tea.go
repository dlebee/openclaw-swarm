package progress

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- Tea messages (package-private, sent via Observer adapter) ---

type teaPhaseStartMsg struct {
	index, total int
	phase        string
}
type teaPhaseEndMsg struct {
	index, total int
	phase        string
	err          error
}
type teaTargetStartMsg struct {
	phase, targetID string
	slot            int
}
type teaTargetEndMsg struct {
	phase, targetID string
	slot            int
	err             error
}
type teaStepStartMsg struct {
	phase, targetID, step string
	slot                  int
}
type teaStepEndMsg struct {
	phase, targetID, step string
	slot                  int
	outcome               CellOutcome
}
type teaFinishMsg struct{ err error }

// --- ExecTea is the public handle returned by NewExecTea ---

// ExecTea wraps a Bubble Tea program that visualises scaffold execution.
type ExecTea struct {
	prog *tea.Program
}

// NewExecTea creates a Bubble Tea execution UI writing to w.
// cancel is invoked on Ctrl+C so the execution context can be cancelled.
// Returns the runner and an Observer to pass to ExecuteOptions.Progress.
func NewExecTea(w io.Writer, cancel context.CancelFunc) (*ExecTea, Observer) {
	m := &execTeaModel{
		cancel:       cancel,
		phaseStepSet: make(map[string]bool),
		barWidth:     56,
	}
	p := tea.NewProgram(m, tea.WithOutput(w))
	return &ExecTea{prog: p}, &teaSender{prog: p}
}

// Run starts the Bubble Tea program and blocks until execution finishes.
// Returns the execution error (not a Tea UI error).
func (et *ExecTea) Run() error {
	final, err := et.prog.Run()
	if err != nil {
		return err
	}
	m, ok := final.(*execTeaModel)
	if !ok {
		return fmt.Errorf("progress: unexpected tea model type %T", final)
	}
	return m.doneErr
}

// Finish signals that scaffold execution completed (ok or error).
func (et *ExecTea) Finish(err error) {
	et.prog.Send(teaFinishMsg{err: err})
}

// --- teaSender implements Observer by forwarding to the tea.Program ---

type teaSender struct{ prog *tea.Program }

func (s *teaSender) OnPhaseStart(index, total int, phase string) {
	s.prog.Send(teaPhaseStartMsg{index: index, total: total, phase: phase})
}
func (s *teaSender) OnPhaseEnd(index, total int, phase string, err error) {
	s.prog.Send(teaPhaseEndMsg{index: index, total: total, phase: phase, err: err})
}
func (s *teaSender) OnTargetStart(phase, targetID string, slot int) {
	s.prog.Send(teaTargetStartMsg{phase: phase, targetID: targetID, slot: slot})
}
func (s *teaSender) OnTargetEnd(phase, targetID string, slot int, err error) {
	s.prog.Send(teaTargetEndMsg{phase: phase, targetID: targetID, slot: slot, err: err})
}
func (s *teaSender) OnStepStart(phase, targetID, step string, slot int) {
	s.prog.Send(teaStepStartMsg{phase: phase, targetID: targetID, step: step, slot: slot})
}
func (s *teaSender) OnStepEnd(phase, targetID, step string, slot int, outcome CellOutcome) {
	s.prog.Send(teaStepEndMsg{phase: phase, targetID: targetID, step: step, slot: slot, outcome: outcome})
}

// --- Bubble Tea model ---

type execSlot struct {
	targetID string
	stepName string
	done     int
}

type execTeaModel struct {
	cancel context.CancelFunc

	phaseIndex int
	phaseTotal int
	phaseName  string

	phaseSteps   []string
	phaseStepSet map[string]bool

	slots     []execSlot
	completed []string

	barWidth int
	doneErr  error
	finished bool
}

func (m *execTeaModel) ensureSlot(slot int) {
	for slot >= len(m.slots) {
		m.slots = append(m.slots, execSlot{})
	}
}

func (m *execTeaModel) Init() tea.Cmd { return nil }

func (m *execTeaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case teaFinishMsg:
		m.doneErr = msg.err
		m.finished = true
		return m, tea.Quit

	case teaPhaseStartMsg:
		m.phaseIndex = msg.index
		m.phaseTotal = msg.total
		m.phaseName = msg.phase
		m.slots = nil
		m.phaseSteps = nil
		m.phaseStepSet = make(map[string]bool)
		return m, nil

	case teaPhaseEndMsg:
		return m, nil

	case teaTargetStartMsg:
		m.ensureSlot(msg.slot)
		m.slots[msg.slot] = execSlot{targetID: msg.targetID}
		return m, nil

	case teaTargetEndMsg:
		m.ensureSlot(msg.slot)
		line := formatDoneLine(msg.targetID, msg.err)
		m.completed = append(m.completed, line)
		m.slots[msg.slot] = execSlot{}
		return m, nil

	case teaStepStartMsg:
		if !m.phaseStepSet[msg.step] {
			m.phaseStepSet[msg.step] = true
			m.phaseSteps = append(m.phaseSteps, msg.step)
		}
		m.ensureSlot(msg.slot)
		m.slots[msg.slot].stepName = msg.step
		return m, nil

	case teaStepEndMsg:
		m.ensureSlot(msg.slot)
		m.slots[msg.slot].done++
		m.slots[msg.slot].stepName = ""
		return m, nil

	case tea.WindowSizeMsg:
		w := msg.Width - 8
		if w < 24 {
			w = 24
		}
		if w > 96 {
			w = 96
		}
		m.barWidth = w
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		return m, nil

	default:
		return m, nil
	}
}

// --- View ---

var (
	execTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	execMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	execWorkerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	execIdleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	execBarStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	execOkStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	execErrStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m *execTeaModel) View() string {
	if m.phaseName == "" && len(m.slots) == 0 {
		return execMutedStyle.Render("Waiting…")
	}

	title := execTitleStyle.Render(fmt.Sprintf("Phase %d/%d · %s", m.phaseIndex, m.phaseTotal, m.phaseName))
	sub := execMutedStyle.Render(fmt.Sprintf("%d workers · %d steps per target", len(m.slots), len(m.phaseSteps)))

	rows := make([]string, 0, len(m.slots)*4+8)
	rows = append(rows, title, sub, "")

	bw := m.barWidth
	if bw < 24 {
		bw = 24
	}

	for i, slot := range m.slots {
		var pct float64
		total := len(m.phaseSteps)
		if total > 0 && slot.targetID != "" {
			pct = float64(slot.done) / float64(total)
		}

		bar := execPercentBar(pct, bw)
		pctLabel := execMutedStyle.Render(fmt.Sprintf(" %3.0f%%", pct*100))
		barRow := lipgloss.JoinHorizontal(lipgloss.Left, bar, pctLabel)

		rows = append(rows,
			execWorkerStyle.Render(fmt.Sprintf("Worker %d", i+1)),
			barRow,
			m.slotStatusLine(i),
		)
		if i < len(m.slots)-1 {
			rows = append(rows, "")
		}
	}

	if len(m.completed) > 0 {
		rows = append(rows, "")
		rows = append(rows, execWorkerStyle.Render("Completed"))
		for _, line := range m.completed {
			rows = append(rows, "  "+line)
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m *execTeaModel) slotStatusLine(i int) string {
	if i >= len(m.slots) {
		return execIdleStyle.Render("(idle)")
	}
	slot := m.slots[i]
	if slot.targetID == "" {
		return execIdleStyle.Render("(idle)")
	}
	if slot.stepName != "" {
		return execMutedStyle.Render(slot.targetID + " — " + slot.stepName)
	}
	return execMutedStyle.Render(slot.targetID)
}

func execPercentBar(pct float64, width int) string {
	if width < 8 {
		width = 8
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(float64(width)*pct + 0.5)
	if filled > width {
		filled = width
	}
	var b strings.Builder
	b.Grow(width + 2)
	b.WriteByte('[')
	for i := 0; i < width; i++ {
		if i < filled {
			b.WriteRune('█')
		} else {
			b.WriteRune('░')
		}
	}
	b.WriteByte(']')
	return execBarStyle.Render(b.String())
}

func formatDoneLine(name string, err error) string {
	if err == nil {
		return execOkStyle.Render("✓ ") + name
	}
	return execErrStyle.Render("✗ ") + name + ": " + err.Error()
}
