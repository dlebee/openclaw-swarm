package scaffold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

type mockStep struct {
	name           string
	applicable     bool
	applicableErr  error
	checkSatisfied bool
	checkErr       error
	executeErr     error
	verifyErr      error
	calls          []string
	mu             sync.Mutex
	onExecuteSleep time.Duration
	onExecuteHook  func()
	// checkFunc, when non-nil, overrides the default Check behavior. Used by
	// tests that need Check to perform real work (e.g. block on a sync gate
	// to prove parallel probing). Defaults to nil so existing tests keep
	// using the simple checkSatisfied / checkErr fields.
	checkFunc func(ctx context.Context) (bool, error)
}

func (m *mockStep) record(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, s)
}

func (m *mockStep) Name() string { return m.name }

func (m *mockStep) Applicable(ctx context.Context, t Target) (bool, error) {
	m.record("applicable")
	if m.applicableErr != nil {
		return false, m.applicableErr
	}
	return m.applicable, nil
}

func (m *mockStep) Check(ctx context.Context, t Target) (bool, error) {
	m.record("check")
	if m.checkFunc != nil {
		return m.checkFunc(ctx)
	}
	if m.checkErr != nil {
		return false, m.checkErr
	}
	return m.checkSatisfied, nil
}

func (m *mockStep) Execute(ctx context.Context, t Target) error {
	m.record("execute")
	if m.onExecuteHook != nil {
		m.onExecuteHook()
	}
	if m.onExecuteSleep > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.onExecuteSleep):
		}
	}
	return m.executeErr
}

func (m *mockStep) Verify(ctx context.Context, t Target) error {
	m.record("verify")
	return m.verifyErr
}

type mockBarrier struct {
	err error
}

func (b *mockBarrier) Evaluate(ctx context.Context, phaseName string, results []CellResult) error {
	return b.err
}

type planningRecorder struct {
	lines []string
	mu    sync.Mutex
}

func (p *planningRecorder) OnPlanning(phase, step string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, fmt.Sprintf("%s/%s", phase, step))
}

func TestBuild_empty(t *testing.T) {
	_, err := New().Build()
	if err == nil || !strings.Contains(err.Error(), "no phases") {
		t.Fatalf("expected no phases error, got %v", err)
	}
}

func TestBuild_validation(t *testing.T) {
	p := New()
	p.AddPhase("")
	_, err := p.Build()
	if err == nil {
		t.Fatal("expected error for empty phase name")
	}

	p2 := New()
	ph := p2.AddPhase("a")
	ph.AddStep(&mockStep{name: "s"})
	_, err = p2.Build()
	if err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Fatalf("expected targets error: %v", err)
	}

	p3 := New()
	ph3 := p3.AddPhase("badprobe")
	ph3.AddTargets(Target{ID: "t"})
	ph3.AddStep(&mockStep{name: "s"})
	ph3.ProbeConcurrency = -1
	_, err = p3.Build()
	if err == nil || !strings.Contains(err.Error(), "probe concurrency") {
		t.Fatalf("expected probe concurrency error: %v", err)
	}
}

func TestDescribe(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	d, err := ex.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "p1") || !strings.Contains(d, "A") || !strings.Contains(d, "t1") {
		t.Fatalf("describe: %q", d)
	}
	if !strings.Contains(d, "will execute") || !strings.Contains(d, "Phase:") || !strings.Contains(d, "Target t1") {
		t.Fatalf("describe should reflect probe and hierarchy: %q", d)
	}
}

func TestDescribeStyled_nonEmpty(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.DescribeStyled(context.Background(), 80)
	if err != nil {
		t.Fatal(err)
	}
	if s == "" || !strings.Contains(s, "p1") {
		t.Fatalf("styled: %q", s)
	}
	if !strings.Contains(s, "Phase:") || !strings.Contains(s, "Target ") || !strings.Contains(s, "t1") {
		t.Fatalf("expected phase/target tree: %q", s)
	}
	if !strings.Contains(s, "will execute") {
		t.Fatalf("expected probed will execute: %q", s)
	}
}

func TestDescribeStyledWithHints_skipPhaseLabelsSkipped(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.DescribeStyledWithHints(context.Background(), 80, PlanDisplayHints{SkipPhases: []string{"p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "skipped") {
		t.Fatalf("want skipped status: %q", s)
	}
}

func TestDescribeStyledWithHints_dryRunShowsWouldExecute(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.DescribeStyledWithHints(context.Background(), 80, PlanDisplayHints{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "would execute") {
		t.Fatalf("want would execute after probe in dry-run: %q", s)
	}
}

func TestDescribeStyledWithProbe_notApplicable(t *testing.T) {
	a := &mockStep{name: "A", applicable: false}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.DescribeStyledWithHints(context.Background(), 80, PlanDisplayHints{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "not applicable") {
		t.Fatalf("want not applicable: %q", s)
	}
}

func TestBuild_firesOnPlanning(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	b := &mockStep{name: "B", applicable: true}
	rec := &planningRecorder{}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ph.AddStep(b)
	_, err := p.Build(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.lines) != 2 {
		t.Fatalf("want 2 planning lines, got %v", rec.lines)
	}
}

func TestExecute_happyPath(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	err = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(a.calls, ","); got != "applicable,check,execute,verify" {
		t.Fatalf("calls: %s", got)
	}
}

func TestExecute_applicable_false(t *testing.T) {
	a := &mockStep{name: "A", applicable: false}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, _ := p.Build()
	_ = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}})
	if got := strings.Join(a.calls, ","); got != "applicable" {
		t.Fatalf("want only applicable, got %q", got)
	}
}

func TestExecute_check_satisfied(t *testing.T) {
	a := &mockStep{name: "A", applicable: true, checkSatisfied: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, _ := p.Build()
	_ = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}})
	if got := strings.Join(a.calls, ","); got != "applicable,check" {
		t.Fatalf("want applicable,check, got %q", got)
	}
}

func TestExecute_force_bypassesCheck(t *testing.T) {
	// checkSatisfied=true means the non-forced path should stop after
	// check. With Force=true we expect check to be skipped entirely and
	// Execute+Verify to run.
	a := &mockStep{name: "A", applicable: true, checkSatisfied: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, _ := p.Build()
	_ = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}, Force: true})
	if got := strings.Join(a.calls, ","); got != "applicable,execute,verify" {
		t.Fatalf("force should skip check; got %q", got)
	}
}

func TestExecute_force_stillRespectsApplicable(t *testing.T) {
	// Applicable=false is a structural gate, not an idempotency probe —
	// Force must not override it.
	a := &mockStep{name: "A", applicable: false}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, _ := p.Build()
	_ = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}, Force: true})
	if got := strings.Join(a.calls, ","); got != "applicable" {
		t.Fatalf("force should still respect applicable=false; got %q", got)
	}
}

func TestExecute_barrier_fail(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ph.Barrier = &mockBarrier{err: errors.New("barrier")}
	ex, _ := p.Build()
	err := ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}})
	if err == nil || !strings.Contains(err.Error(), "barrier") {
		t.Fatalf("expected barrier error, got %v", err)
	}
}

func TestExecute_concurrency(t *testing.T) {
	pl := New()
	ph := pl.AddPhase("p1")
	ph.Concurrency = 2
	var running, peak int32
	hook := func() {
		n := atomic.AddInt32(&running, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&running, -1)
	}
	step := &mockStep{name: "S", applicable: true, onExecuteHook: hook}
	ph.AddTargets(Target{ID: "t1"}, Target{ID: "t2"}, Target{ID: "t3"})
	ph.AddStep(step)
	ex, err := pl.Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&peak) > 2 {
		t.Fatalf("peak concurrent = %d, want <= 2", peak)
	}
}

func TestExecute_dryRun(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	ex, _ := p.Build()
	_ = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}, DryRun: true})
	if len(a.calls) != 0 {
		t.Fatalf("dry run should not call step, got %v", a.calls)
	}
}

func TestExecute_skipPhase(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph1 := p.AddPhase("p1")
	ph1.AddTargets(Target{ID: "t1"})
	ph1.AddStep(a)
	ph2 := p.AddPhase("p2")
	ph2.AddTargets(Target{ID: "t2"})
	ph2.AddStep(a)
	ex, _ := p.Build()
	_ = ex.Execute(context.Background(), ExecuteOptions{Progress: progress.Noop{}, SkipPhases: []string{"p2"}})
	if !strings.Contains(strings.Join(a.calls, ","), "execute") {
		t.Fatal("expected some execute")
	}
	if n := strings.Count(strings.Join(a.calls, ","), "execute"); n != 1 {
		t.Fatalf("want 1 execute, got calls %v", a.calls)
	}
}

// TestExecute_onlyPhases: a non-empty OnlyPhases list restricts execution to
// the listed phases. Every other phase is skipped (not even probed).
func TestExecute_onlyPhases(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	b := &mockStep{name: "B", applicable: true}
	c := &mockStep{name: "C", applicable: true}
	p := New()
	ph1 := p.AddPhase("p1")
	ph1.AddTargets(Target{ID: "t1"})
	ph1.AddStep(a)
	ph2 := p.AddPhase("p2")
	ph2.AddTargets(Target{ID: "t2"})
	ph2.AddStep(b)
	ph3 := p.AddPhase("p3")
	ph3.AddTargets(Target{ID: "t3"})
	ph3.AddStep(c)

	ex, _ := p.Build()
	err := ex.Execute(context.Background(), ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"p1", "p3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(a.calls, ","), "execute") {
		t.Fatalf("p1 should have executed: %v", a.calls)
	}
	if strings.Contains(strings.Join(b.calls, ","), "applicable") {
		t.Fatalf("p2 should have been skipped entirely, got %v", b.calls)
	}
	if !strings.Contains(strings.Join(c.calls, ","), "execute") {
		t.Fatalf("p3 should have executed: %v", c.calls)
	}
}

// TestExecute_onlyPhasesIntersectSkip: SkipPhases is applied AFTER OnlyPhases,
// so asking to run {p1,p2} but skip {p2} runs only p1.
func TestExecute_onlyPhasesIntersectSkip(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	b := &mockStep{name: "B", applicable: true}
	p := New()
	ph1 := p.AddPhase("p1")
	ph1.AddTargets(Target{ID: "t1"})
	ph1.AddStep(a)
	ph2 := p.AddPhase("p2")
	ph2.AddTargets(Target{ID: "t2"})
	ph2.AddStep(b)

	ex, _ := p.Build()
	err := ex.Execute(context.Background(), ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"p1", "p2"},
		SkipPhases: []string{"p2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(a.calls, ","), "execute") {
		t.Fatalf("p1 should have executed: %v", a.calls)
	}
	if strings.Contains(strings.Join(b.calls, ","), "applicable") {
		t.Fatalf("p2 should have been skipped by deny-list: %v", b.calls)
	}
}

// TestDescribeStyledWithHints_onlyPhases: phases not in OnlyPhases render as
// "skipped (phase)" in the prepared-plan tree, same as SkipPhases.
func TestDescribeStyledWithHints_onlyPhases(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph1 := p.AddPhase("p1")
	ph1.AddTargets(Target{ID: "t1"})
	ph1.AddStep(a)
	ph2 := p.AddPhase("p2")
	ph2.AddTargets(Target{ID: "t2"})
	ph2.AddStep(a)
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.DescribeStyledWithHints(context.Background(), 80, PlanDisplayHints{
		OnlyPhases: []string{"p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "Only phases: p1.") {
		t.Fatalf("want only-phases header note, got: %q", s)
	}
	if !strings.Contains(s, "skipped") {
		t.Fatalf("want skipped status for p2 cell, got: %q", s)
	}
}

func TestPhaseNames(t *testing.T) {
	p := New()
	ph1 := p.AddPhase("p1")
	ph1.AddTargets(Target{ID: "t1"})
	ph1.AddStep(&mockStep{name: "A", applicable: true})
	ph2 := p.AddPhase("p2")
	ph2.AddTargets(Target{ID: "t2"})
	ph2.AddStep(&mockStep{name: "B", applicable: true})
	got := p.PhaseNames()
	if len(got) != 2 || got[0] != "p1" || got[1] != "p2" {
		t.Fatalf("Plan.PhaseNames: %v", got)
	}
	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	got = ex.PhaseNames()
	if len(got) != 2 || got[0] != "p1" || got[1] != "p2" {
		t.Fatalf("ExecutablePlan.PhaseNames: %v", got)
	}
}

func TestExecWithConfirm_declined(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	err := ExecWithConfirm(context.Background(), p, PipelineOptions{
		Confirm: func() (bool, error) { return false, nil },
		Out:     io.Discard,
		Width:   80,
	})
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("want ErrDeclined, got %v", err)
	}
	if strings.Contains(strings.Join(a.calls, ","), "execute") {
		t.Fatalf("execute should not run, got %v", a.calls)
	}
}

func TestExecWithConfirm_dryRunSkipsConfirmAndExecute(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	var confirmCalls int
	err := ExecWithConfirm(context.Background(), p, PipelineOptions{
		Confirm: func() (bool, error) {
			confirmCalls++
			return false, nil
		},
		Out:   io.Discard,
		Width: 80,
		ExecuteOptions: ExecuteOptions{
			Progress: progress.Noop{},
			DryRun:   true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if confirmCalls != 0 {
		t.Fatalf("confirm should not run on dry-run, got %d calls", confirmCalls)
	}
	if strings.Contains(strings.Join(a.calls, ","), "execute") {
		t.Fatalf("execute path should not run on dry-run, got calls %v", a.calls)
	}
	if !strings.Contains(strings.Join(a.calls, ","), "applicable") {
		t.Fatalf("dry-run plan should still probe applicable, got %v", a.calls)
	}
}

func TestExecWithConfirm_fullPipeline(t *testing.T) {
	a := &mockStep{name: "A", applicable: true}
	p := New()
	ph := p.AddPhase("p1")
	ph.AddTargets(Target{ID: "t1"})
	ph.AddStep(a)
	var order []string
	rec := &planningRecorder{}
	err := ExecWithConfirm(context.Background(), p, PipelineOptions{
		BuildProgress: rec,
		Confirm:       func() (bool, error) { order = append(order, "confirm"); return true, nil },
		Out:           io.Discard,
		Width:         80,
		ExecuteOptions: ExecuteOptions{
			Progress: progress.Noop{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.lines) != 1 {
		t.Fatalf("planning lines %v", rec.lines)
	}
	if len(order) != 1 || order[0] != "confirm" {
		t.Fatalf("confirm order %v", order)
	}
	got := strings.Join(a.calls, ",")
	want := "applicable,check,applicable,check,execute,verify"
	if got != want {
		t.Fatalf("want probe then runCell chain %q, got %q", want, got)
	}
}
