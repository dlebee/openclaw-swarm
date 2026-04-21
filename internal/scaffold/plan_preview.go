package scaffold

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var planPreviewHelp = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

// RunPlanPreview shows the probed plan in an alternate-screen Bubble Tea
// viewport. Quit with q, esc, or ctrl+c. Returns a ProbeSummary aggregating
// the cell verdicts so the caller (ExecWithConfirm) can short-circuit the
// confirm + Execute phase when nothing needs to run.
func RunPlanPreview(ctx context.Context, exec *ExecutablePlan, hints PlanDisplayHints) (ProbeSummary, error) {
	if exec == nil {
		return ProbeSummary{}, fmt.Errorf("scaffold: nil executable plan")
	}
	ctx = EnsurePlanCache(ctx)
	SetProbeActive(ctx, true)
	defer SetProbeActive(ctx, false)
	cells, err := annotatePlanCellsWithProbe(ctx, exec.compiled, hints)
	if err != nil {
		return ProbeSummary{}, err
	}
	summary := summariseCells(cells)
	p := tea.NewProgram(
		newPlanPreviewModel(hints, cells),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		return summary, err
	}
	return summary, nil
}

type planPreviewModel struct {
	hints PlanDisplayHints
	cells []annotatedCell
	vp    viewport.Model
	ready bool
}

func newPlanPreviewModel(hints PlanDisplayHints, cells []annotatedCell) planPreviewModel {
	return planPreviewModel{hints: hints, cells: cells, vp: viewport.New(0, 0)}
}

func (m planPreviewModel) Init() tea.Cmd { return nil }

func (m planPreviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.ready = true
		footer := m.footerView(msg.Width)
		fh := lipgloss.Height(footer)
		m.vp.Width = msg.Width
		m.vp.Height = max(1, msg.Height-fh)
		m.vp.SetContent(renderPreparedPlanTree(m.cells, msg.Width, m.hints))
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m planPreviewModel) footerView(width int) string {
	line := planPreviewHelp.Render(" ↑/↓/PgUp/PgDn · mouse wheel · q esc quit ")
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, line)
}

func (m planPreviewModel) View() string {
	if !m.ready {
		return "Loading…"
	}
	w := m.vp.Width
	if w <= 0 {
		w = 80
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		m.vp.View(),
		m.footerView(w),
	)
}
