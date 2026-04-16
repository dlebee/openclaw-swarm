package scaffold

import (
	"fmt"
	"os"

	bubblesprogress "github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	scaffoldprogress "github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

type planningLineMsg struct {
	phase, step string
}

type buildFinishedMsg struct {
	exec *ExecutablePlan
	err  error
}

type spinnerBuildObserver struct {
	send func(tea.Msg)
}

func (s *spinnerBuildObserver) OnPlanning(phase, step string) {
	s.send(planningLineMsg{phase: phase, step: step})
}

type planningBuildModel struct {
	spinner      spinner.Model
	progress     bubblesprogress.Model
	barFraction  float64 // 0..1; rendered via ViewAs so it matches phase without spring frames
	totalPhases  int
	phaseIndex   map[string]int
	currentPhase string
	exec         *ExecutablePlan
	buildErr     error
}

func phaseCompileFraction(phaseIndex0, totalPhases int) float64 {
	if totalPhases < 1 {
		return 1
	}
	return float64(phaseIndex0+1) / float64(totalPhases)
}

func newPlanningBuildModel(p *Plan) *planningBuildModel {
	n := len(p.Phases)
	idx := make(map[string]int, n)
	for i, ph := range p.Phases {
		idx[ph.Name] = i
	}

	bar := bubblesprogress.New(
		bubblesprogress.WithDefaultGradient(),
		bubblesprogress.WithWidth(44),
	)
	return &planningBuildModel{
		spinner: spinner.New(
			spinner.WithSpinner(spinner.MiniDot),
			spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("205"))),
		),
		progress:    bar,
		totalPhases: n,
		phaseIndex:  idx,
	}
}

func (m *planningBuildModel) Init() tea.Cmd {
	return func() tea.Msg {
		return m.spinner.Tick()
	}
}

func (m *planningBuildModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case buildFinishedMsg:
		m.exec = msg.exec
		m.buildErr = msg.err
		m.barFraction = 1
		return m, tea.Quit

	case planningLineMsg:
		if idx, ok := m.phaseIndex[msg.phase]; ok {
			m.currentPhase = msg.phase
			m.barFraction = phaseCompileFraction(idx, m.totalPhases)
			sm, sc := m.spinner.Update(msg)
			m.spinner = sm
			return m, sc
		}

	case tea.WindowSizeMsg:
		w := msg.Width - 4
		if w < 20 {
			w = 20
		}
		if w > 120 {
			w = 120
		}
		m.progress.Width = w
	}

	sm, sc := m.spinner.Update(msg)
	m.spinner = sm
	return m, sc
}

func (m *planningBuildModel) View() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")).Render("Planning....")
	row1 := lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View(), " ", title)

	// ViewAs: spring animation never gets FrameMsg ticks if we quit in the same
	// batch as SetPercent, so the label would stick at 0%. Snap to barFraction instead.
	bar := m.progress.ViewAs(m.barFraction)
	var rows []string
	rows = append(rows, row1, bar)
	if m.currentPhase != "" {
		sub := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(m.currentPhase)
		rows = append(rows, sub)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// runPlanningBuildWithSpinner runs Plan.Build with Bubble Tea on stderr: a
// spinner + "Planning....", a phase-based progress bar, then the program exits
// so the caller can print the plan on stdout.
func runPlanningBuildWithSpinner(p *Plan) (*ExecutablePlan, error) {
	m := newPlanningBuildModel(p)
	prog := tea.NewProgram(m, tea.WithOutput(os.Stderr))

	go func() {
		ob := &spinnerBuildObserver{send: prog.Send}
		ex, err := p.Build(ob)
		prog.Send(buildFinishedMsg{exec: ex, err: err})
	}()

	final, err := prog.Run()
	if err != nil {
		return nil, err
	}
	fm, ok := final.(*planningBuildModel)
	if !ok {
		return nil, fmt.Errorf("scaffold: unexpected planning spinner model type %T", final)
	}
	return fm.exec, fm.buildErr
}

func buildPlanWithProgress(p *Plan, obs scaffoldprogress.BuildObserver) (*ExecutablePlan, error) {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		exec, err := runPlanningBuildWithSpinner(p)
		_, _ = fmt.Fprintln(os.Stderr)
		return exec, err
	}
	return p.Build(obs)
}
