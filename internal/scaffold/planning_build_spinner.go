package scaffold

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync/atomic"

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

// probeCellStartMsg is routed from OnProbeStart into the Tea loop when a
// probe goroutine is about to call Applicable+Check for a given cell.
type probeCellStartMsg struct {
	phase, targetID, step string
	seq                   uint64 // monotonic; drives insertion order of in-flight list
}

// probeCellEndMsg is routed from OnProbeEnd when a probe finishes. The Tea
// model uses the tuple (phase, targetID, step) to move the cell out of
// "in flight" and bump the completed counter.
type probeCellEndMsg struct {
	phase, targetID, step string
}

// probeFinishedMsg delivers the probe's final result back to the Tea event
// loop so we can exit cleanly and hand the rendered tree (or error) back to
// the caller.
type probeFinishedMsg struct {
	tree string
	err  error
}

// spinnerProbeObserver forwards probe lifecycle events to a Tea program.
// It's thread-safe: prog.Send is safe to call from multiple goroutines, and
// the only mutable state is an atomic sequence counter used to preserve
// relative arrival order of concurrent probe starts (Tea's own message
// queue ordering is sufficient in practice, but the seq makes the
// in-flight breadcrumb list stable even if two messages race into the
// channel at the same instant).
type spinnerProbeObserver struct {
	send func(tea.Msg)
	seq  atomic.Uint64
}

// OnProbeStart implements ProbeObserver.
func (s *spinnerProbeObserver) OnProbeStart(phase, targetID, step string) {
	n := s.seq.Add(1)
	s.send(probeCellStartMsg{phase: phase, targetID: targetID, step: step, seq: n})
}

// OnProbeEnd implements ProbeObserver.
func (s *spinnerProbeObserver) OnProbeEnd(phase, targetID, step string, _ cellStatusKind) {
	s.send(probeCellEndMsg{phase: phase, targetID: targetID, step: step})
}

// probeInflight is one row in the "in flight" list. seq keeps the
// display order stable (insertion order) even when cells complete out of
// order.
type probeInflight struct {
	phase, targetID, step string
	seq                   uint64
}

func probeKey(phase, targetID, step string) string {
	return phase + "\x00" + targetID + "\x00" + step
}

type planProbeModel struct {
	spinner    spinner.Model
	total      int
	completed  int
	inflight   map[string]probeInflight
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
		total:    total,
		inflight: make(map[string]probeInflight),
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
			m.completed = m.total
		}
		m.inflight = map[string]probeInflight{}
		return m, tea.Quit

	case probeCellStartMsg:
		key := probeKey(msg.phase, msg.targetID, msg.step)
		m.inflight[key] = probeInflight{phase: msg.phase, targetID: msg.targetID, step: msg.step, seq: msg.seq}
		sm, sc := m.spinner.Update(msg)
		m.spinner = sm
		return m, sc

	case probeCellEndMsg:
		key := probeKey(msg.phase, msg.targetID, msg.step)
		if _, ok := m.inflight[key]; ok {
			delete(m.inflight, key)
			m.completed++
		}
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
	styleProbeCount   = lipgloss.NewStyle().Foreground(lipgloss.Color("111"))
	styleProbePhase   = lipgloss.NewStyle().Foreground(lipgloss.Color("111")).Bold(true)
	styleProbeTarget  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleProbeStep    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleProbeSep     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleProbeBullet  = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
)

// maxInflightRows caps how many breadcrumb rows we render. Plans with high
// phase concurrency (default 4) rarely exceed a handful in flight, but this
// guards against walls-of-text if someone sets ph.concurrency very high.
const maxInflightRows = 8

func (m *planProbeModel) View() string {
	title := styleProbeTitle.Render("Probing plan....")

	// Counter line: "N/total (K in flight)" when we know the total,
	// otherwise just the in-flight count (defensive for edge cases).
	counter := ""
	if m.total > 0 {
		counter = styleProbeCounter.Render(
			fmt.Sprintf("  %s/%s", styleProbeCount.Render(fmt.Sprint(m.completed)), fmt.Sprint(m.total)),
		)
	}
	inflightCount := len(m.inflight)
	if inflightCount > 0 {
		counter += styleProbeCounter.Render(fmt.Sprintf("  (%d in flight)", inflightCount))
	}

	row1 := lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View(), " ", title, counter)

	rows := []string{row1}
	if inflightCount > 0 {
		breadcrumbs := sortedInflight(m.inflight)
		shown := breadcrumbs
		truncated := 0
		if len(shown) > maxInflightRows {
			truncated = len(shown) - maxInflightRows
			shown = shown[:maxInflightRows]
		}
		for _, c := range shown {
			bullet := styleProbeBullet.Render("·")
			sep := styleProbeSep.Render(" · ")
			line := bullet + " " +
				styleProbePhase.Render(c.phase) + sep +
				styleProbeTarget.Render(c.targetID) + sep +
				styleProbeStep.Render(c.step)
			rows = append(rows, "  "+line)
		}
		if truncated > 0 {
			rows = append(rows, "  "+styleProbeSep.Render(fmt.Sprintf("… +%d more", truncated)))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// sortedInflight returns the in-flight map as a list ordered by insertion seq
// so the breadcrumb list doesn't jitter as items enter/leave.
func sortedInflight(m map[string]probeInflight) []probeInflight {
	out := make([]probeInflight, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// runPlanProbeWithSpinner runs exec.DescribeStyledWithHintsObs inside a Tea
// program, rendering "Probing plan.... N/total (K in flight)" + per-cell
// breadcrumbs on stderr while cells probe in parallel. Returns the rendered
// plan tree (or the probe error). When stderr isn't a TTY we fall back to a
// plain synchronous probe with no UI.
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
	_, _ = fmt.Fprintln(os.Stderr)
	return fm.treeOutput, fm.probeErr
}
