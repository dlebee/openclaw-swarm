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
	checkBlocked   bool
	checkErr       error
	executeErr     error
	verifyErr      error
	calls          []string
	mu             sync.Mutex
	onExecuteSleep time.Duration
	onExecuteHook  func()
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
	if m.checkErr != nil {
		return false, m.checkErr
	}
	return m.checkBlocked, nil
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

func TestExecute_check_blocked(t *testing.T) {
	a := &mockStep{name: "A", applicable: true, checkBlocked: true}
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
