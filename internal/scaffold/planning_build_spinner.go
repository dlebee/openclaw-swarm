package scaffold

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	scaffoldprogress "github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

// buildPlanWithProgress compiles the plan. Compilation is in-memory and takes
// milliseconds, so there's no Tea UI here — the interesting progress surface
// is the probe pass (see runPlanProbeWithSpinner), which hits SSH / multipass /
// provider APIs and can stall for minutes. We keep the BuildObserver hook for
// non-TTY callers (logs, tests) that still want compile-time callbacks.
func buildPlanWithProgress(p *Plan, obs scaffoldprogress.BuildObserver) (*ExecutablePlan, error) {
	if obs == nil {
		obs = scaffoldprogress.NoopBuild{}
	}
	return p.Build(obs)
}

// ------------------------------------------------------------------
// Probe spinner
// ------------------------------------------------------------------
//
// probeCellMsg is routed into the Tea program from annotatePlanCellsWithProbeObs
// (running in a goroutine). Each arrival means the probe goroutine is ABOUT TO
// call Applicable/Check for that cell — so when the UI displays phase+step, it
// accurately reflects what's currently blocking the probe, not what already
// completed.
type probeCellMsg struct {
	index, total          int
	phase, targetID, step string
}

// probeFinishedMsg delivers the probe's final result back to the Tea event
// loop so we can exit cleanly and hand the rendered tree (or error) back to
// the caller.
type probeFinishedMsg struct {
	tree string
	err  error
}

type spinnerProbeObserver struct {
	send func(tea.Msg)
}

// OnProbeStart implements ProbeObserver by forwarding to the Tea program. We
// deliberately send BEFORE the probe's Applicable/Check call so a hang is
// attributable to the displayed cell.
func (s *spinnerProbeObserver) OnProbeStart(index, total int, phase, targetID, step string) {
	s.send(probeCellMsg{index: index, total: total, phase: phase, targetID: targetID, step: step})
}

type planProbeModel struct {
	spinner    spinner.Model
	total      int
	current    int
	phaseName  string
	targetID   string
	stepName   string
	treeOutput string
	probeErr   error
	done       bool
}

func newPlanProbeModel(total int) *planProbeModel {
	return &planProbeModel{
		spinner: spinner.New(
			spinner.WithSpinner(spinner.MiniDot),
			spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("205"))),
		),
		total: total,
	}
}

func (m *planProbeModel) Init() tea.Cmd {
	return func() tea.Msg { return m.spinner.Tick() }
}

func (m *planProbeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case probeFinishedMsg:
		m.done = true
		m.treeOutput = msg.tree
		m.probeErr = msg.err
		if m.total > 0 {
			m.current = m.total
		}
		return m, tea.Quit

	case probeCellMsg:
		if msg.total > 0 {
			m.total = msg.total
		}
		m.current = msg.index
		m.phaseName = msg.phase
		m.targetID = msg.targetID
		m.stepName = msg.step
		sm, sc := m.spinner.Update(msg)
		m.spinner = sm
		return m, sc
	}

	sm, sc := m.spinner.Update(msg)
	m.spinner = sm
	return m, sc
}

var (
	styleProbeTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleProbeCounter = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleProbePhase   = lipgloss.NewStyle().Foreground(lipgloss.Color("111")).Bold(true)
	styleProbeTarget  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleProbeStep    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

func (m *planProbeModel) View() string {
	title := styleProbeTitle.Render("Probing plan....")
	counter := ""
	if m.total > 0 {
		counter = styleProbeCounter.Render(fmt.Sprintf("  %d/%d", m.current, m.total))
	}
	row1 := lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View(), " ", title, counter)

	rows := []string{row1}
	if m.phaseName != "" || m.stepName != "" {
		parts := []string{}
		if m.phaseName != "" {
			parts = append(parts, styleProbePhase.Render(m.phaseName))
		}
		if m.targetID != "" {
			parts = append(parts, styleProbeTarget.Render(m.targetID))
		}
		if m.stepName != "" {
			parts = append(parts, styleProbeStep.Render(m.stepName))
		}
		sub := ""
		for i, p := range parts {
			if i > 0 {
				sub += styleProbeStep.Render(" · ")
			}
			sub += p
		}
		rows = append(rows, "  "+sub)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// runPlanProbeWithSpinner runs exec.DescribeStyledWithHintsObs inside a Tea
// program, rendering "Probing plan.... N/total · phase · target · step" on
// stderr while each cell's Applicable+Check runs. Returns the rendered plan
// tree (or the probe error). When stderr isn't a TTY we fall back to a plain
// synchronous probe with no UI.
func runPlanProbeWithSpinner(ctx context.Context, exec *ExecutablePlan, width int, h PlanDisplayHints) (string, error) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return exec.DescribeStyledWithHints(ctx, width, h)
	}

	m := newPlanProbeModel(exec.ProbeCellCount(h))
	prog := tea.NewProgram(m, tea.WithOutput(os.Stderr))

	go func() {
		obs := &spinnerProbeObserver{send: prog.Send}
		tree, err := exec.DescribeStyledWithHintsObs(ctx, width, h, obs)
		prog.Send(probeFinishedMsg{tree: tree, err: err})
	}()

	final, err := prog.Run()
	if err != nil {
		return "", err
	}
	fm, ok := final.(*planProbeModel)
	if !ok {
		return "", fmt.Errorf("scaffold: unexpected probe spinner model type %T", final)
	}
	// Drop a trailing newline so the prepared-plan tree doesn't crowd
	// against the spinner's final frame.
	_, _ = fmt.Fprintln(os.Stderr)
	return fm.treeOutput, fm.probeErr
}
