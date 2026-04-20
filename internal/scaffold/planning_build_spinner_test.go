package scaffold

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type recordingProbeObserver struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingProbeObserver) OnProbeStart(index, total int, phase, targetID, step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf("%d/%d %s/%s/%s", index, total, phase, targetID, step))
}

func TestAnnotatePlanCellsWithProbeObs_FiresForEveryProbedCell(t *testing.T) {
	p := New()
	ph := p.AddPhase("probe-phase")
	ph.AddTargets(Target{ID: "alpha"}, Target{ID: "beta"})
	ph.AddStep(&mockStep{name: "s1", applicable: true, checkSatisfied: true})
	ph.AddStep(&mockStep{name: "s2", applicable: true})

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	obs := &recordingProbeObserver{}
	if _, err := ex.DescribeStyledWithHintsObs(context.Background(), 80, PlanDisplayHints{}, obs); err != nil {
		t.Fatalf("describe: %v", err)
	}

	want := []string{
		"1/4 probe-phase/alpha/s1",
		"2/4 probe-phase/alpha/s2",
		"3/4 probe-phase/beta/s1",
		"4/4 probe-phase/beta/s2",
	}
	if len(obs.lines) != len(want) {
		t.Fatalf("events: got %d %v, want %d %v", len(obs.lines), obs.lines, len(want), want)
	}
	for i, exp := range want {
		if obs.lines[i] != exp {
			t.Fatalf("event %d: got %q, want %q", i, obs.lines[i], exp)
		}
	}
}

func TestProbeCellCount_ExcludesFilteredPhases(t *testing.T) {
	p := New()
	ph1 := p.AddPhase("keep")
	ph1.AddTargets(Target{ID: "t"})
	ph1.AddStep(&mockStep{name: "s1"})
	ph1.AddStep(&mockStep{name: "s2"})

	ph2 := p.AddPhase("drop")
	ph2.AddTargets(Target{ID: "t"})
	ph2.AddStep(&mockStep{name: "s"})

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	if got := ex.ProbeCellCount(PlanDisplayHints{}); got != 3 {
		t.Fatalf("unfiltered: got %d, want 3", got)
	}
	if got := ex.ProbeCellCount(PlanDisplayHints{SkipPhases: []string{"drop"}}); got != 2 {
		t.Fatalf("skip drop: got %d, want 2", got)
	}
	if got := ex.ProbeCellCount(PlanDisplayHints{OnlyPhases: []string{"drop"}}); got != 1 {
		t.Fatalf("only drop: got %d, want 1", got)
	}
}

func TestPlanProbeModel_AdvancesOnCellEvents(t *testing.T) {
	m := newPlanProbeModel(3)

	m.Update(probeCellMsg{index: 1, total: 3, phase: "provisioning", targetID: "gateway-host", step: "create-machine"})
	if m.current != 1 || m.phaseName != "provisioning" || m.targetID != "gateway-host" || m.stepName != "create-machine" {
		t.Fatalf("after cell 1: current=%d phase=%q target=%q step=%q", m.current, m.phaseName, m.targetID, m.stepName)
	}

	m.Update(probeCellMsg{index: 2, total: 3, phase: "provisioning", targetID: "gateway-host", step: "authorize-ssh-key"})
	m.Update(probeCellMsg{index: 3, total: 3, phase: "mesh-join", targetID: "node-host", step: "install-tailscale"})
	if m.current != 3 {
		t.Fatalf("current after 3 cells: got %d, want 3", m.current)
	}

	view := m.View()
	if !strings.Contains(view, "3/3") {
		t.Fatalf("view missing counter 3/3: %q", view)
	}
	for _, needle := range []string{"mesh-join", "node-host", "install-tailscale"} {
		if !strings.Contains(view, needle) {
			t.Fatalf("view missing %q: %q", needle, view)
		}
	}
}

func TestPlanProbeModel_FinishSnapsToTotal(t *testing.T) {
	m := newPlanProbeModel(5)
	m.Update(probeCellMsg{index: 2, total: 5, phase: "a", targetID: "t", step: "s"})
	m.Update(probeFinishedMsg{tree: "TREE", err: nil})
	if !m.done {
		t.Fatal("done flag not set after probeFinishedMsg")
	}
	if m.treeOutput != "TREE" {
		t.Fatalf("treeOutput: got %q, want TREE", m.treeOutput)
	}
	if m.current != m.total {
		t.Fatalf("finish must snap counter to total: got %d, want %d", m.current, m.total)
	}
}

func TestPlanProbeModel_CounterOmittedWhenTotalUnknown(t *testing.T) {
	m := newPlanProbeModel(0)
	v := m.View()
	if strings.Contains(v, "0/0") {
		t.Fatalf("view should not render 0/0 counter: %q", v)
	}
}
