# Scaffold Execution Model

## Core Concepts

- **Plan**: Mutable builder (`scaffold.Plan`) that holds phases. Call `Build()` to validate and produce an `ExecutablePlan`.
- **Phase**: An ordered group of targets and steps. Phases execute sequentially. Each phase has a configurable concurrency limit (default 4).
- **Step**: One unit of work that implements the `Step` interface. Steps run sequentially per target.
- **Target**: A concrete object a step operates on. Targets within a phase run concurrently (up to `Phase.Concurrency`).
- **Cell**: The intersection of one target and one step — the smallest unit of execution.
- **Barrier**: Optional aggregate check after all targets in a phase complete. If the barrier fails, the plan stops before the next phase.

---

## Step Interface

Every step implements:

```go
type Step interface {
    Name() string
    Applicable(ctx context.Context, t Target) (bool, error)
    Check(ctx context.Context, t Target) (satisfied bool, err error)
    Execute(ctx context.Context, t Target) error
    Verify(ctx context.Context, t Target) error
}
```

| Method       | Purpose                                                        |
| ------------ | -------------------------------------------------------------- |
| `Applicable` | Should this step run for this target at all?                   |
| `Check`      | Is the desired state already satisfied? If yes, skip Execute.  |
| `Execute`    | Perform the work to reach desired state.                       |
| `Verify`     | Confirm the desired state was reached after Execute.           |

---

## Execution Matrix

A phase is a matrix of steps over targets:

```
              Target 1    Target 2    Target 3
  Step A      Cell        Cell        Cell
  Step B      Cell        Cell        Cell
```

Targets fan out concurrently; steps within a single target pipeline run sequentially.

---

## Cell Lifecycle

Each cell runs:

```
applicable? ──no──▶ skip
    │
   yes
    ▼
  check ──satisfied──▶ done (no work needed)
    │
 not satisfied
    ▼
  execute
    │
    ▼
  verify
```

---

## Pipeline

```
Plan.Build() ──▶ probe (Applicable + Check per cell) ──▶ preview tree ──▶ confirm ──▶ execute
```

**Preview (prepared plan)** runs `Applicable` and `Check` for every cell (except phases in `skip_phases`), then renders what would happen:

| Status           | Meaning                                              |
| ---------------- | ---------------------------------------------------- |
| `will execute`   | Applicable, not yet satisfied — will run Execute.    |
| `would execute`  | Same, but dry-run mode — Execute would run.          |
| `satisfied`      | Check passed — desired state already exists.         |
| `not applicable` | Step does not apply to this target.                  |
| `skipped (phase)`| Phase is in `skip_phases`.                           |

`Execute` is never called during preview. `Verify` runs only after a real `Execute`.

---

## Run Options

```yaml
run_options:
  dry_run: false
  skip_phases: []
```

- **dry_run**: Build the plan, show the probed preview, then exit. No confirm prompt, no Execute/Verify. Cells that would run show as `would execute`.
- **skip_phases**: Skip execution and probing of listed phases. Those cells appear as `skipped (phase)` in the preview.

---

## Pseudocode

### Full Pipeline

```
function run(plan, options):
    executable = plan.Build()
    preview = probe(executable, options)     # Applicable + Check per cell
    render(preview)

    if options.dry_run:
        return

    confirm_or_abort()

    for each phase in executable.phases:
        if phase.name in options.skip_phases:
            continue

        fan out targets (up to phase.concurrency):
            for each step in phase.steps:
                result = run_cell(target, step)
                if result.err:
                    stop target pipeline

        if phase.barrier:
            barrier.Evaluate(all_cell_results)
```

### Cell

```
function run_cell(target, step):
    if not step.Applicable(target):
        return skipped

    if step.Check(target).satisfied:
        return satisfied

    step.Execute(target)
    step.Verify(target)
    return ok
```

---

## One-Line Summary

The scaffold builds a plan of sequential phases, each fanning out targets concurrently through a sequential step pipeline, where every cell runs applicable → check → execute → verify, and a preview probe runs only applicable + check to show what would happen.
