package manifestcmd

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

var helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

type viewportModel struct {
	displayPath string
	manifest    *data.Manifest
	vp          viewport.Model
	ready       bool
}

func newViewportModel(displayPath string, m *data.Manifest) viewportModel {
	vp := viewport.New(0, 0)
	return viewportModel{
		displayPath: displayPath,
		manifest:    m,
		vp:          vp,
	}
}

func (m viewportModel) Init() tea.Cmd {
	return nil
}

func (m viewportModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.ready = true
		footerH := lipgloss.Height(m.footerView())
		m.vp.Width = msg.Width
		m.vp.Height = max(1, msg.Height-footerH)
		m.vp.SetContent(RenderManifest(m.displayPath, m.manifest, msg.Width))
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

func (m viewportModel) footerView() string {
	line := helpStyle.Render(" ↑/↓/PgUp/PgDn · mouse wheel · q esc quit ")
	return lipgloss.PlaceHorizontal(m.vp.Width, lipgloss.Center, line)
}

func (m viewportModel) View() string {
	if !m.ready {
		return "Loading…"
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		m.vp.View(),
		m.footerView(),
	)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RunViewport runs the interactive scrollable viewer (alternate screen).
func RunViewport(displayPath string, m *data.Manifest) error {
	p := tea.NewProgram(
		newViewportModel(displayPath, m),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}

// PlainText is RenderManifest for non-TTY output.
func PlainText(displayPath string, m *data.Manifest) string {
	return RenderManifest(displayPath, m, 100)
}
