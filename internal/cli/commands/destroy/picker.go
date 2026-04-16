package destroy

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	clawdestroy "github.com/gluwa/openclaw-swarm2/internal/claws/plans/destroy"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).MarginBottom(1)
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).MarginTop(1)
)

type pickerModel struct {
	items   []hosting.Instance
	cursor  int
	sel     map[int]struct{}
	aborted bool
	done    bool
}

func (m pickerModel) selectedInstances() []hosting.Instance {
	var out []hosting.Instance
	for i := range m.items {
		if _, ok := m.sel[i]; ok {
			out = append(out, m.items[i])
		}
	}
	return out
}

func (m pickerModel) Init() tea.Cmd { return nil }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.aborted = true
			return m, tea.Quit
		case "q":
			if m.done {
				return m, nil
			}
			m.aborted = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case " ":
			if _, ok := m.sel[m.cursor]; ok {
				delete(m.sel, m.cursor)
			} else {
				m.sel[m.cursor] = struct{}{}
			}
		case "enter":
			if len(m.sel) > 0 {
				m.done = true
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m pickerModel) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Select instances to destroy (space toggles, enter confirms, esc cancels)"))
	b.WriteString("\n\n")
	for i, inst := range m.items {
		mark := "[ ]"
		if _, ok := m.sel[i]; ok {
			mark = "[x]"
		}
		cursor := "  "
		if m.cursor == i {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%s %s\n", cursor, mark, clawdestroy.FormatInstanceLine(inst))
		if m.cursor == i {
			line = lipgloss.NewStyle().Bold(true).Render(strings.TrimSuffix(line, "\n")) + "\n"
		}
		b.WriteString(line)
	}
	b.WriteString(helpStyle.Render("space: toggle · enter: destroy selected · esc/q: cancel"))
	return b.String()
}

// PickInstancesMulti runs an interactive multi-select on a TTY. Returns abort error if user cancels
// or confirms nothing. Caller should pass non-empty items.
func PickInstancesMulti(items []hosting.Instance) ([]hosting.Instance, error) {
	if len(items) == 0 {
		return nil, nil
	}
	m := pickerModel{
		items: items,
		sel:   make(map[int]struct{}),
	}
	p := tea.NewProgram(m, tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return nil, err
	}
	pm, ok := final.(pickerModel)
	if !ok {
		return nil, fmt.Errorf("destroy picker: unexpected model type")
	}
	if pm.aborted && !pm.done {
		return nil, fmt.Errorf("cancelled")
	}
	if !pm.done {
		return nil, fmt.Errorf("cancelled")
	}
	return pm.selectedInstances(), nil
}
