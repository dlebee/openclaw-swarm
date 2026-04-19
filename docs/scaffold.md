# Scaffold — Mental Model and Spec

A single-file reference for the `internal/scaffold` library: what it is, why it
exists, the mental model you should carry in your head, and the precise contract
it enforces at runtime.

---

## 1. What scaffold is

Scaffold is a small in-process orchestrator for **idempotent, declarative
work over a matrix of targets**. You describe *what* needs to be true (phases of
steps over targets), and scaffold decides *how little* to do to make it true,
then shows the user a preview before doing it.

You get, for free:

- A structured plan you can preview before executing.
- Concurrency control per phase with sequential per-target pipelines.
- Uniform lifecycle hooks (Applicable → Check → Execute → Verify) so every
  operation becomes idempotent in the same shape.
- A plan-scoped cache shared by probe and execute so you don't re-query the
  world twice.
- Pluggable progress observers (silent, line logger, Bubble Tea UI).
- Optional per-phase barriers for cross-target invariants.
- A built-in `ExecWithConfirm` pipeline: build → preview → confirm → execute.

It is **not** a DAG engine, job queue, or distributed workflow system. Phases
are a linear sequence; targets inside a phase fan out but do not depend on each
other.

---

## 2. Mental model in one picture

```
Plan
└── Phase (sequential)
    ├── Targets      ── fan out, up to Phase.Concurrency workers
    │   └── Steps    ── run sequentially per target
    │       └── Cell ── Applicable → Check → Execute → Verify
    └── Barrier      ── runs after all targets complete
```

Read it top-down:

- A **Plan** is a list of phases.
- A **Phase** is a matrix: one row per target, one column per step.
- **Targets** run concurrently (bounded by `Phase.Concurrency`, default `4`).
- **Steps** for a given target run **sequentially** — step 2 only starts if
  step 1 didn't fail.
- A **Cell** is the intersection of one target and one step; it is the atomic
  unit and always runs the same four-method lifecycle.
- A **Barrier** is an optional aggregate check that runs after all cells in a
  phase are done; if it fails, subsequent phases don't run.

```
                       Target 1   Target 2   Target 3
           Step A        Cell       Cell       Cell
           Step B        Cell       Cell       Cell
           Step C        Cell       Cell       Cell
                          ▼          ▼          ▼
                       finish ──► Barrier ──► next phase
```

---

## 3. Core concepts

### 3.1 Plan

Mutable builder. You add phases, then `Build()` validates the graph and returns
an `ExecutablePlan`. Plans are not meant to be mutated after `Build`.

### 3.2 Phase

An ordered group of targets and steps, plus a concurrency limit and optional
barrier. Phases are executed in the order they were added.

### 3.3 Step

One unit of work implementing the `Step` interface. A step applies to every
target in the phase — it is "the thing we want to be true", expressed once.

### 3.4 Target

A concrete object the step operates on (a machine, a DNS record, a volume…).
Has an `ID` (used for logs and progress) and an opaque `Payload`.

### 3.5 Cell

The runtime intersection of (Target, Step). Every cell runs the same lifecycle;
its `CellResult` records what happened.

### 3.6 Barrier

Aggregate invariant over all cell results in a phase. Typical uses: "at least
one node is reachable", "quorum established", "all certs present".

### 3.7 Plan cache

A `context`-scoped key/value map plus a list of `io.Closer` resources, shared
across all cells of one run (probe + execute). Lets `Check` stash a probe
result that `Execute` and later steps reuse instead of hitting the world twice.

---

## 4. The Step contract (the heart of everything)

```go
type Step interface {
    Name() string
    Applicable(ctx context.Context, t Target) (bool, error)
    Check(ctx context.Context, t Target) (satisfied bool, err error)
    Execute(ctx context.Context, t Target) error
    Verify(ctx context.Context, t Target) error
}
```

| Method       | Answers                                                      | Side effects |
| ------------ | ------------------------------------------------------------ | ------------ |
| `Applicable` | Does this step make sense for this target at all?            | None         |
| `Check`      | Is the desired state already in place? If yes, skip Execute. | Read-only    |
| `Execute`    | Do the work needed to reach the desired state.               | Yes          |
| `Verify`     | Confirm the desired state after Execute.                     | Read-only    |

Rules of the road:

- `Applicable` and `Check` must be **side-effect free** — they run during the
  preview probe before the user has confirmed anything.
- `Execute` is the **only** mutating method. It runs only after `Applicable=true`
  and `Check=not satisfied`, and only after the user confirms (or the caller
  skips confirm explicitly).
- `Verify` runs **only after a real `Execute`**, never during the preview, never
  when `Check` was already satisfied. It's a post-condition, not a probe.
- `Name()` must be non-empty and is used as the step id in plan output and
  progress events.

### 4.1 Cell lifecycle

```
Applicable ──false──▶ skip (result.Skipped = true)
   │
  true
   ▼
 Check ──satisfied──▶ done (result.Satisfied = true, no Execute)
   │
 not satisfied
   ▼
 Execute ──err──▶ fail (stop this target's pipeline)
   │
   ▼
 Verify  ──err──▶ fail
   │
   ▼
 ok
```

Any error stops that target's step pipeline, but other targets in the same
phase keep running. The phase still reports the **first** target error at the
end.

---

## 5. Pipeline lifecycle

```
┌─────────┐    ┌───────┐    ┌────────┐    ┌──────────┐    ┌────────┐    ┌──────┐
│  Plan   │──▶│ Build │──▶│ Probe  │──▶│ Preview  │──▶│ Confirm│──▶│ Exec │
└─────────┘    └───────┘    └────────┘    └──────────┘    └────────┘    └──────┘
 authoring     validate     Applicable    render tree    user/auto      Execute
 (mutable)     + compile    + Check       to terminal    yes/no         + Verify
                            per cell                                     per cell
```

- **Build**: validates the plan (non-empty phases, non-empty names, at least one
  step and target per phase, concurrency ≥ 1, non-nil steps) and fires
  `BuildObserver.OnPlanning` once per (phase, step).
- **Probe**: for every cell, runs `Applicable` then (if applicable) `Check`.
  Does not mutate.
- **Preview**: renders a Lip Gloss tree (`Phase → Target → step + status`)
  showing what would happen.
- **Confirm**: the caller-supplied `Confirm` function gates execution. Default
  is "yes" when none is supplied.
- **Exec**: runs the actual cell lifecycle per (target, step).

Dry-run mode collapses this to `Build → Probe → Preview → return`. No confirm,
no `Execute`, no `Verify`.

---

## 6. Execution semantics

- **Phases are sequential.** Phase *n+1* only starts if phase *n* finished
  without error and its barrier passed.
- **Targets within a phase are concurrent.** `Phase.Concurrency` is the max
  number of in-flight targets (default 4, must be ≥ 1). Each target is assigned
  a stable worker `slot` in `[0, Concurrency)`.
- **Steps within a target are sequential.** Step *k+1* runs only if step *k*
  didn't return an error (including context cancellation).
- **First target error wins.** `executePhase` collects all cell results, then
  returns the first non-nil target error, but still runs peers to completion.
- **Barrier on success.** The phase barrier, if present, only runs if no target
  errored; its error becomes the phase error (`barrier %q: <err>`).
- **Hard stop on phase error.** Any phase error aborts the plan; later phases
  don't run.
- **Context cancellation** is propagated: cells already running keep going
  until their `Execute` returns, but no new steps start on a cancelled target.

---

## 7. Preview / probe semantics

The preview is the contract between the operator and the automation. Every
status you can see has an exact meaning:

| Status             | Meaning                                                        |
| ------------------ | -------------------------------------------------------------- |
| `will execute`     | Applicable, Check not satisfied → Execute will run.            |
| `would execute`    | Same as above, but in dry-run mode; Execute would run.         |
| `satisfied`        | Check passed; nothing to do for this cell.                     |
| `not applicable`   | `Applicable` returned false for this target.                   |
| `skipped (phase)`  | Phase is listed in `SkipPhases`; not probed and not executed.  |
| `applicable: <e>`  | `Applicable` returned an error during probe.                   |
| `check: <e>`       | `Check` returned an error during probe.                        |

Key invariants:

- Probe is read-only. If your `Applicable` or `Check` mutates, the preview
  lies and idempotency breaks.
- Probe runs even in dry-run. Dry-run only changes **labels and what runs
  after** (no Execute, no confirm).
- `SkipPhases` applies to both probe and execute — skipped phases are not
  contacted at all.

---

## 8. Plan cache (context-scoped state)

Every plan run gets a `planCache` attached to the `context.Context` via
`EnsurePlanCache`. The same cache is shared by probe, execute, and every
cell — it is the sanctioned way to avoid duplicate external queries across the
lifecycle.

Low-level API:

```go
ctx = scaffold.EnsurePlanCache(ctx)          // idempotent
scaffold.PlanCacheSet(ctx, "foo", 42)
v, ok := scaffold.PlanCacheGet(ctx, "foo")
b, ok := scaffold.PlanCacheBool(ctx, "bar")
scaffold.RegisterPlanCloser(ctx, conn)       // closed at end of run
```

Resource hygiene:

- `runPlan` calls `ClosePlanResources(ctx)` in a `defer`, so anything registered
  via `RegisterPlanCloser` is cleaned up automatically when the plan finishes,
  whether success or error.
- The cache is safe for concurrent use by multiple targets in the same phase.

High-level helpers (machine-specific, shipped with scaffold):

```go
scaffold.RecordPlanMachineExists(ctx, targetID, exists)
exists, known := scaffold.DoesMachineExist(ctx, targetID)

scaffold.RecordPlanMachineHost(ctx, machineName, host)
host, ok := scaffold.LookupPlanMachineHost(ctx, machineName)
scaffold.ForgetPlanMachineHost(ctx, machineName)  // destroy flows
```

Rule of thumb: never read the cache by raw key outside the scaffold package;
use typed helpers so the key space stays stable.

---

## 9. Progress observers

Execution is observable without changing plan code.

```go
type Observer interface {
    OnPhaseStart(index, total int, phase string)
    OnPhaseEnd(index, total int, phase string, err error)
    OnTargetStart(phase, targetID string, slot int)
    OnTargetEnd(phase, targetID string, slot int, err error)
    OnStepStart(phase, targetID, step string, slot int)
    OnStepEnd(phase, targetID, step string, slot int, outcome CellOutcome)
}

type BuildObserver interface {
    OnPlanning(phase, step string)   // fires once per (phase, step) during Build
}
```

Built-ins in `scaffold/progress`:

- `progress.Noop` / `progress.NoopBuild` — silent.
- Styled line logger for non-TTY output.
- `progress.NewExecTea` — Bubble Tea UI with per-worker bars and completion
  marks; wired automatically when `PipelineOptions.TeaProgressWriter` is set.

Slot semantics: each in-flight target is assigned a stable worker slot in
`[0, Concurrency)` and reported via `On*` events, so UIs can render one row per
worker.

---

## 10. Public API surface

### 10.1 Types

```go
type Plan struct { Phases []*Phase }
type Phase struct {
    Name        string
    Targets     []Target
    Steps       []Step
    Concurrency int            // default 4, must be ≥ 1
    Barrier     Barrier        // optional
}
type Target struct { ID string; Payload any }

type ExecutablePlan // produced by Plan.Build()

type Step interface { /* §4 */ }
type Barrier interface {
    Evaluate(ctx context.Context, phaseName string, results []CellResult) error
}

type CellResult struct {
    TargetID, StepName string
    Skipped, Satisfied bool
    Err                error
}
```

### 10.2 Authoring

```go
p := scaffold.New()
ph := p.AddPhase("bootstrap")
ph.Concurrency = 8
ph.AddTargets(scaffold.Target{ID: "node-a"}, scaffold.Target{ID: "node-b"})
ph.AddStep(mySSHStep)
ph.AddStep(myPairStep)
ph.Barrier = myQuorumBarrier

exec, err := p.Build(buildObs...)         // validates + fires OnPlanning
```

### 10.3 Describe / preview

```go
text, err := exec.Describe(ctx)                                // plain text
styled, err := exec.DescribeStyled(ctx, width)                 // Lip Gloss tree
styled, err := exec.DescribeStyledWithHints(ctx, width, hints) // + DryRun/SkipPhases
```

### 10.4 Execute

```go
err := exec.Execute(ctx, scaffold.ExecuteOptions{
    Progress:   progressObs,   // nil → Noop
    DryRun:     false,         // true: probe only, no Execute/Verify
    SkipPhases: []string{"destroy"},
})
```

### 10.5 Full pipeline with confirm

```go
err := scaffold.ExecWithConfirm(ctx, p, scaffold.PipelineOptions{
    BuildProgress: spinner,
    Confirm:       func() (bool, error) { return promptYN(), nil },
    Out:           os.Stdout,
    Width:         80,
    PrettyPlan:    true,              // Bubble Tea viewport on TTY
    TeaProgressWriter: os.Stdout,     // optional Bubble Tea exec UI
    ExecuteOptions: scaffold.ExecuteOptions{ /* DryRun, SkipPhases, … */ },
})
// errors.Is(err, scaffold.ErrDeclined) when the user says no
```

### 10.6 Escape hatches / utilities

- `scaffold.NoopStep{StepName: "placeholder"}` — use in phases that don't have
  a real step yet, so `Build` validation passes.
- `scaffold.PlanDisplayHints{DryRun, SkipPhases}` — tweak preview copy without
  changing execution.

---

## 11. Run options in YAML (surface example)

```yaml
run_options:
  dry_run: false
  skip_phases: []
```

- **`dry_run`**: Build, probe, render preview, exit. No confirm, no Execute,
  no Verify. Cells that would run show as `would execute`.
- **`skip_phases`**: Names of phases to skip entirely (probe and execute).
  Cells in those phases render as `skipped (phase)`.

These map directly to `ExecuteOptions.DryRun` and `ExecuteOptions.SkipPhases`.

---

## 12. Writing a correct Step — checklist

- [ ] `Name()` is short, stable, lowercase-dashed (e.g. `authorize-ssh-key`).
- [ ] `Applicable` answers a static question about the target (shape,
      capability, feature flag), **never** touches the outside world in a way
      that changes it.
- [ ] `Check` is a pure read. Its result can and should be cached on the plan
      cache when the same data is needed in later cells.
- [ ] `Execute` is the only place with side effects. It assumes `Applicable=true`
      and `Check=not satisfied`.
- [ ] `Verify` confirms the post-condition. If the system is eventually
      consistent, poll with a context-aware timeout.
- [ ] Every method respects `ctx.Done()` and returns promptly on cancellation.
- [ ] Errors are wrapped with enough context to identify the target.
- [ ] Concurrency-safe: the step struct may be called from multiple goroutines
      (one per target).

---

## 13. Pseudocode, end to end

```text
function run(plan, options):
    executable = plan.Build()             # validate, fire OnPlanning

    ctx = EnsurePlanCache(ctx)
    preview = probe(executable, options)  # Applicable + Check per cell
    render(preview)

    if options.dry_run:
        return

    if not confirm():
        return ErrDeclined

    for each phase in executable.phases:
        if phase.name in options.skip_phases: continue

        results = fan_out(phase.targets, up_to=phase.concurrency,
                          each_target: run_target_pipeline(phase.steps))

        if phase.barrier:
            if err := phase.barrier.Evaluate(results): abort(err)

    ClosePlanResources(ctx)

function run_target_pipeline(steps, target):
    for step in steps:
        if ctx.Done(): break
        result = run_cell(target, step)
        if result.Err: break

function run_cell(target, step):
    if not step.Applicable(target): return skipped
    if step.Check(target).satisfied: return satisfied
    step.Execute(target)
    step.Verify(target)
    return ok
```

---

## 14. One-line summary

Scaffold runs a sequence of phases; each phase fans out targets through a
sequential step pipeline whose every cell is `Applicable → Check → Execute →
Verify`, with an up-front read-only probe rendering exactly what will happen
before the user confirms.
