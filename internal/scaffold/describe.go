package scaffold

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

var (
	descPhaseStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	descStepStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	descMutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

func describeStyled(compiled []compiledPhase, width int) string {
	if width < 40 {
		width = 80
	}
	var parts []string
	for _, ph := range compiled {
		header := descPhaseStyle.Render(fmt.Sprintf("phase %q", ph.name))
		sub := descMutedStyle.Render(fmt.Sprintf("concurrency=%d", ph.concurrency))
		parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, header, sub))
		for _, st := range ph.steps {
			stepLine := descStepStyle.Render(fmt.Sprintf("step %q", st.name))
			meta := fmt.Sprintf("%d cells", len(st.cells))
			if st.barrier != nil {
				meta += " [barrier]"
			}
			parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, stepLine, descMutedStyle.Render(meta)))
			rows := make([][]string, 0, len(st.cells))
			for _, c := range st.cells {
				rows = append(rows, []string{c.action.Name(), c.target.ID})
			}
			if len(rows) > 0 {
				t := table.New().
					Border(lipgloss.RoundedBorder()).
					BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
					Headers("action", "target").
					Width(width).
					Rows(rows...)
				parts = append(parts, t.String())
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
