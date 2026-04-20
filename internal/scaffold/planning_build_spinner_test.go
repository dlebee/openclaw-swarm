package scaffold

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type recordingProbeObserver struct {
	mu       sync.Mutex
	starts   []string
	ends     []string
	inflight int
	peakLive int
}

func (r *recordingProbeObserver) OnProbeStart(phase, targetID, step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, fmt.Sprintf("%s/%s/%s", phase, targetID, step))
	r.inflight++
	if r.inflight > r.peakLive {
		r.peakLive = r.inflight
	}
}

func (r *recordingProbeObserver) OnProbeEnd(phase, targetID, step string, _ cellStatusKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, fmt.Sprintf("%s/%s/%s", phase, targetID, step))
	r.inflight--
}

func (r *recordingProbeObserver) peak() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peakLive
}

func TestAnnotatePlanCellsWithProbeObs_FiresLifecycleForEveryCell(t *testing.T) {
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

	// Every cell fires Start and End exactly once. Targets and steps run
	// in plan order (sequential probe).
	want := map[string]int{
		"probe-phase/alpha/s1": 1,
		"probe-phase/alpha/s2": 1,
		"probe-phase/beta/s1":  1,
		"probe-phase/beta/s2":  1,
	}
	if got := tallyLines(obs.starts); !equalTally(got, want) {
		t.Fatalf("starts tally: got %v, want %v", got, want)
	}
	if got := tallyLines(obs.ends); !equalTally(got, want) {
		t.Fatalf("ends tally: got %v, want %v", got, want)
	}
}

func TestAnnotatePlanCellsWithProbeObs_ForceSkipsCheckAndLabelsWillExecute(t *testing.T) {
	// Satisfied-by-check step. Without Force, the probe should call
	// Check and label the cell satisfied. With Force=true, Check must
	// NOT be called at all and the probe should label it "will execute".
	a := &mockStep{name: "only", applicable: true, checkSatisfied: true}
	p := New()
	ph := p.AddPhase("phase")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	obs := &recordingProbeObserver{}
	if _, err := ex.DescribeStyledWithHintsObs(context.Background(), 80, PlanDisplayHints{Force: true}, obs); err != nil {
		t.Fatalf("describe: %v", err)
	}
	a.mu.Lock()
	calls := strings.Join(a.calls, ",")
	a.mu.Unlock()
	if strings.Contains(calls, "check") {
		t.Fatalf("force probe must not call Check; got calls=%q", calls)
	}
	if !strings.Contains(calls, "applicable") {
		t.Fatalf("force probe must still call Applicable; got calls=%q", calls)
	}
}

func TestAnnotatePlanCellsWithProbeObs_AllCellsFireOnce(t *testing.T) {
	// Verify every cell fires exactly once and ends exactly once.
	p := New()
	ph := p.AddPhase("phase")
	ph.AddTargets(Target{ID: "t1"}, Target{ID: "t2"}, Target{ID: "t3"})
	ph.AddStep(&mockStep{name: "s1", applicable: true})
	ph.AddStep(&mockStep{name: "s2", applicable: true})
	ph.Concurrency = 10

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	obs := &recordingProbeObserver{}
	if _, err := ex.DescribeStyledWithHintsObs(context.Background(), 80, PlanDisplayHints{}, obs); err != nil {
		t.Fatalf("describe: %v", err)
	}

	want := map[string]int{
		"phase/t1/s1": 1, "phase/t1/s2": 1,
		"phase/t2/s1": 1, "phase/t2/s2": 1,
		"phase/t3/s1": 1, "phase/t3/s2": 1,
	}
	if got := tallyLines(obs.starts); !equalTally(got, want) {
		t.Fatalf("starts tally: got %v, want %v", got, want)
	}
	if got := tallyLines(obs.ends); !equalTally(got, want) {
		t.Fatalf("ends tally: got %v, want %v", got, want)
	}
}

func TestAnnotatePlanCellsWithProbeObs_ProbesTargetsSequentially(t *testing.T) {
	const nTargets = 4

	p := New()
	ph := p.AddPhase("phase")
	targets := make([]Target, 0, nTargets)
	for i := 0; i < nTargets; i++ {
		targets = append(targets, Target{ID: fmt.Sprintf("t%d", i)})
	}
	ph.AddTargets(targets...)
	ph.AddStep(&mockStep{name: "s1", applicable: true, checkSatisfied: true})
	ph.Concurrency = nTargets

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	obs := &recordingProbeObserver{}
	if _, err := ex.DescribeStyledWithHintsObs(context.Background(), 80, PlanDisplayHints{}, obs); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if obs.peak() != 1 {
		t.Fatalf("peak in-flight: got %d, want 1 (one probe cell at a time)", obs.peak())
	}
	var order []string
	for _, line := range obs.starts {
		parts := strings.Split(line, "/")
		if len(parts) != 3 {
			t.Fatalf("unexpected start line %q", line)
		}
		order = append(order, parts[1])
	}
	want := []string{"t0", "t1", "t2", "t3"}
	if len(order) != len(want) {
		t.Fatalf("starts: len %d, want %d: %v", len(order), len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("target order at %d: got %q want %q (full=%v)", i, order[i], want[i], order)
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

// --- Tea model tests -------------------------------------------------------

func TestPlanProbeModel_TracksInFlightAndCompleted(t *testing.T) {
	m := newPlanProbeModel(3)

	m.Update(probeCellStartMsg{phase: "p", targetID: "a", step: "s1", seq: 1})
	m.Update(probeCellStartMsg{phase: "p", targetID: "b", step: "s1", seq: 2})
	if len(m.inflight) != 2 || m.completed != 0 {
		t.Fatalf("after 2 starts: inflight=%d completed=%d", len(m.inflight), m.completed)
	}

	m.Update(probeCellEndMsg{phase: "p", targetID: "a", step: "s1"})
	if len(m.inflight) != 1 || m.completed != 1 {
		t.Fatalf("after 1 end: inflight=%d completed=%d", len(m.inflight), m.completed)
	}

	view := m.View()
	if !strings.Contains(view, "1/3") {
		t.Fatalf("view missing counter 1/3: %q", view)
	}
	if !strings.Contains(view, "(1 in flight)") {
		t.Fatalf("view missing in-flight label: %q", view)
	}
	if !strings.Contains(view, "b") || !strings.Contains(view, "s1") {
		t.Fatalf("view missing breadcrumb for b/s1: %q", view)
	}
	if strings.Contains(view, "a · s1") {
		t.Fatalf("view should not show completed cell a/s1: %q", view)
	}
}

func TestPlanProbeModel_InflightStableOrder(t *testing.T) {
	m := newPlanProbeModel(5)
	m.Update(probeCellStartMsg{phase: "p", targetID: "alpha", step: "s1", seq: 1})
	m.Update(probeCellStartMsg{phase: "p", targetID: "bravo", step: "s1", seq: 2})
	m.Update(probeCellStartMsg{phase: "p", targetID: "charlie", step: "s1", seq: 3})

	got := sortedInflight(m.inflight)
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i, exp := range want {
		if got[i].targetID != exp {
			t.Fatalf("order[%d]: got %q, want %q", i, got[i].targetID, exp)
		}
	}
}

func TestPlanProbeModel_FinishSnapsToTotal(t *testing.T) {
	m := newPlanProbeModel(5)
	m.Update(probeCellStartMsg{phase: "a", targetID: "t", step: "s", seq: 1})
	m.Update(probeFinishedMsg{tree: "TREE", err: nil})
	if !m.done {
		t.Fatal("done flag not set after probeFinishedMsg")
	}
	if m.treeOutput != "TREE" {
		t.Fatalf("treeOutput: got %q, want TREE", m.treeOutput)
	}
	if m.completed != m.total {
		t.Fatalf("finish must snap completed to total: got %d, want %d", m.completed, m.total)
	}
	if len(m.inflight) != 0 {
		t.Fatalf("finish must clear in-flight: got %d", len(m.inflight))
	}
}

func TestPlanProbeModel_TruncatesLongInflight(t *testing.T) {
	m := newPlanProbeModel(100)
	for i := 0; i < maxInflightRows+3; i++ {
		m.Update(probeCellStartMsg{
			phase:    "p",
			targetID: fmt.Sprintf("t%d", i),
			step:     "s",
			seq:      uint64(i + 1),
		})
	}
	view := m.View()
	if !strings.Contains(view, fmt.Sprintf("+%d more", 3)) {
		t.Fatalf("view missing truncation marker: %q", view)
	}
}

// --- helpers ---------------------------------------------------------------

func tallyLines(lines []string) map[string]int {
	out := make(map[string]int, len(lines))
	for _, l := range lines {
		out[l]++
	}
	return out
}

func equalTally(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
