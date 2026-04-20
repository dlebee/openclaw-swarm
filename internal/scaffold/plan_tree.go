package scaffold

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/tree"
)

// PlanDisplayHints tweaks prepared-plan copy.
//
//   - DryRun only affects labels: cells that pass Applicable+Check show
//     "would execute" instead of "will execute".
//   - OnlyPhases (when non-empty) restricts probing to phases in the list.
//     Filtered phases render each cell as "skipped (phase)".
//   - SkipPhases subtracts phases from the run; filtered cells render as
//     "skipped (phase)" as well. Applied in addition to OnlyPhases.
//
// The same filter semantics are used by execution (see ExecuteOptions), so
// the preview matches what `apply` will actually do.
type PlanDisplayHints struct {
	DryRun     bool
	OnlyPhases []string
	SkipPhases []string

	// Force mirrors ExecuteOptions.Force. When true, the probe skips
	// Check for every applicable cell and labels it "will execute" (or
	// "would execute" under DryRun). This keeps the preview honest —
	// without it, the user could see "satisfied" in the plan and then
	// watch Execute run anyway because Check was bypassed at exec time.
	Force bool
}

type cellStatusKind int

const (
	cellStatusPhaseSkipped cellStatusKind = iota
	cellStatusNotApplicable
	cellStatusSatisfied
	cellStatusWillExecute
	cellStatusWouldExecute
	cellStatusApplicableErr
	cellStatusCheckErr
)

type annotatedCell struct {
	phase, step, targetID string
	seq                   int
	kind                  cellStatusKind
	detail                string
	// applicableDur / checkDur are wall time for Applicable and Check during probe.
	checkRan      bool
	applicableDur time.Duration
	checkDur      time.Duration
}

// planTargetSegment is one target under a phase with ordered cells (steps).
type planTargetSegment struct {
	id    string
	cells []annotatedCell
}

// planPhaseSegment is one phase with ordered targets.
type planPhaseSegment struct {
	phase   string
	targets []planTargetSegment
}

type phaseAgg struct {
	order  []string
	tcells map[string][]annotatedCell
	seen   map[string]struct{}
}

// planSegmentsFromCells groups probed cells as Phase → Target → steps, merging all
// steps for the same target under one Target node (targets ordered by first appearance).
func planSegmentsFromCells(cells []annotatedCell) []planPhaseSegment {
	sorted := append([]annotatedCell(nil), cells...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].seq < sorted[j].seq })

	phaseOrder := []string{}
	byPhase := make(map[string]*phaseAgg)

	for _, c := range sorted {
		a, ok := byPhase[c.phase]
		if !ok {
			a = &phaseAgg{
				tcells: make(map[string][]annotatedCell),
				seen:   make(map[string]struct{}),
			}
			byPhase[c.phase] = a
			phaseOrder = append(phaseOrder, c.phase)
		}
		if _, ok := a.seen[c.targetID]; !ok {
			a.seen[c.targetID] = struct{}{}
			a.order = append(a.order, c.targetID)
		}
		a.tcells[c.targetID] = append(a.tcells[c.targetID], c)
	}

	out := make([]planPhaseSegment, 0, len(phaseOrder))
	for _, phName := range phaseOrder {
		a := byPhase[phName]
		tgts := make([]planTargetSegment, 0, len(a.order))
		for _, tid := range a.order {
			row := append([]annotatedCell(nil), a.tcells[tid]...)
			sort.Slice(row, func(i, j int) bool { return row[i].seq < row[j].seq })
			tgts = append(tgts, planTargetSegment{id: tid, cells: row})
		}
		out = append(out, planPhaseSegment{phase: phName, targets: tgts})
	}
	return out
}

// stepDisplayName turns scaffold step ids into short plan labels.
func stepDisplayName(step string) string {
	switch step {
	case "authorize-ssh-key":
		return "authorized keys"
	default:
		return strings.ReplaceAll(step, "-", " ")
	}
}

var (
	planTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	planSubtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	planPhaseLabel  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	planPhaseName   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	planTargetLabel = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	planTargetID    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	planStepStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	statusExecuteOK = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	statusWouldOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	statusMuted     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	statusWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	statusErr       = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func planTreeRootTitle(h PlanDisplayHints) string {
	title := planTitleStyle.Render("Prepared plan")
	var notes []string
	switch {
	case h.DryRun:
		notes = append(notes, "Dry-run: preview runs Applicable and Check only. Execute and Verify are not run (Verify runs only after a real Execute).")
	default:
		notes = append(notes, "Preview runs Applicable and Check only. After you confirm, each cell may Execute then Verify.")
	}
	if len(h.OnlyPhases) > 0 {
		notes = append(notes, fmt.Sprintf("Only phases: %s.", strings.Join(h.OnlyPhases, ", ")))
	}
	if len(h.SkipPhases) > 0 {
		notes = append(notes, fmt.Sprintf("Skip phases: %s.", strings.Join(h.SkipPhases, ", ")))
	}
	sub := planSubtleStyle.Render(strings.Join(notes, " "))
	return lipgloss.JoinVertical(lipgloss.Left, title, sub)
}

func statusStyleForKind(k cellStatusKind) lipgloss.Style {
	switch k {
	case cellStatusWillExecute:
		return statusExecuteOK
	case cellStatusWouldExecute:
		return statusWouldOK
	case cellStatusNotApplicable, cellStatusPhaseSkipped:
		return statusMuted
	case cellStatusSatisfied:
		return statusWarn
	case cellStatusApplicableErr, cellStatusCheckErr:
		return statusErr
	default:
		return lipgloss.NewStyle()
	}
}

const maxStatusDetail = 72

func cellStatusText(c annotatedCell) string {
	switch c.kind {
	case cellStatusPhaseSkipped:
		return "skipped (phase)"
	case cellStatusNotApplicable:
		return "not applicable"
	case cellStatusSatisfied:
		return "satisfied"
	case cellStatusWillExecute:
		return "will execute"
	case cellStatusWouldExecute:
		return "would execute"
	case cellStatusApplicableErr:
		return truncateStatusDetail("applicable: ", c.detail)
	case cellStatusCheckErr:
		return truncateStatusDetail("check: ", c.detail)
	default:
		return ""
	}
}

func truncateStatusDetail(prefix, detail string) string {
	s := prefix + detail
	if len(s) <= maxStatusDetail {
		return s
	}
	if len(prefix) >= maxStatusDetail-3 {
		return prefix[:maxStatusDetail-3] + "..."
	}
	room := maxStatusDetail - len(prefix) - 3
	if room < 1 {
		return s[:maxStatusDetail-3] + "..."
	}
	if len(detail) <= room {
		return s
	}
	return prefix + detail[:room] + "..."
}

func phaseRootLine(phase string) string {
	return lipgloss.JoinHorizontal(lipgloss.Left,
		planPhaseLabel.Render("Phase:"),
		"  ",
		planPhaseName.Render(phase),
	)
}

func targetRootLine(targetID string) string {
	return lipgloss.JoinHorizontal(lipgloss.Left,
		planTargetLabel.Render("Target "),
		planTargetID.Render(targetID),
	)
}

func formatProbeDuration(d time.Duration) string {
	if d <= 0 {
		return "0ms"
	}
	rd := d.Round(time.Millisecond)
	if rd < time.Millisecond && d > 0 {
		return "<1ms"
	}
	return rd.String()
}

func formatCheckLabel(c annotatedCell) string {
	if c.checkRan {
		return formatProbeDuration(c.checkDur)
	}
	return "—"
}

func probeTimingSuffixStyled(c annotatedCell) string {
	if c.kind == cellStatusPhaseSkipped {
		return ""
	}
	s := fmt.Sprintf("app %s  chk %s", formatProbeDuration(c.applicableDur), formatCheckLabel(c))
	return "  " + planSubtleStyle.Render(s)
}

// StepTimingPlain returns probe timings for plain-text plan output (no ANSI).
func StepTimingPlain(c annotatedCell) string {
	if c.kind == cellStatusPhaseSkipped {
		return ""
	}
	return fmt.Sprintf("  app %s  chk %s", formatProbeDuration(c.applicableDur), formatCheckLabel(c))
}

func stepStatusLine(c annotatedCell) string {
	stepSt := planStepStyle.Render(stepDisplayName(c.step))
	sep := "  "
	tag := statusStyleForKind(c.kind).Render(cellStatusText(c))
	timing := probeTimingSuffixStyled(c)
	return lipgloss.JoinHorizontal(lipgloss.Left, stepSt, sep, tag, timing)
}

// renderPreparedPlanTree renders probed cells as Phase → Target → step + status.
func renderPreparedPlanTree(cells []annotatedCell, width int, h PlanDisplayHints) string {
	if width < 40 {
		width = 80
	}
	segs := planSegmentsFromCells(cells)

	tr := tree.New().
		Enumerator(tree.RoundedEnumerator).
		Root(planTreeRootTitle(h))

	for _, ph := range segs {
		phSub := tree.New().
			Enumerator(tree.RoundedEnumerator).
			Root(phaseRootLine(ph.phase))
		for _, tg := range ph.targets {
			tgSub := tree.New().
				Enumerator(tree.RoundedEnumerator).
				Root(targetRootLine(tg.id))
			for _, c := range tg.cells {
				tgSub.Child(stepStatusLine(c))
			}
			phSub.Child(tgSub)
		}
		tr.Child(phSub)
	}

	body := tr.String()
	return lipgloss.NewStyle().Width(width).MaxWidth(width).Render(body)
}
