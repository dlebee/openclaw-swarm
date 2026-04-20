# Plan apply steps — authoring rules

This tree implements `scaffold.Step` for every phase of `claws apply`. Steps are invoked in two distinct passes:

1. **Probe** — read-only, strictly sequential within each phase: targets in plan order, then steps in plan order per target (so earlier Checks can hydrate shared payloads before later ones). `Phase.Concurrency` applies only to **Execute**, not probe.
2. **Execute** — sequential within a target, concurrent across targets within a phase. `Execute` and `Verify` do the real work.

## Applicable and Check are pure predicates

A cold cache and a hot cache MUST produce the same verdict.

**Do not** from inside `Applicable` or `Check`:

- Write the plan cache (`PlanCacheSet`, `RecordPlanMachine*`).
- Mutate the payload (`mt.Instance = ...`, etc.).
- Swallow errors as `false, nil` — return `false, err` so the preview can distinguish "unsatisfied" from "unknown".

If your step needs world state to decide, call a `Resolve*()` helper and read the result.

## Resolve helpers: prefer the cache, don't depend on it

```go
func getOrResolveControlURL(ctx context.Context, dial SSHDialFunc, mt *MeshTarget) (string, error) {
    if v, ok := scaffold.PlanCacheGet(ctx, CacheKeyControlURL); ok {
        if s, _ := v.(string); s != "" {
            return s, nil
        }
    }
    url, err := resolveControlURL(ctx, dial, mt, mt.Gateway)
    if err != nil {
        return "", fmt.Errorf("resolve control URL: %w", err)
    }
    scaffold.PlanCacheSet(ctx, CacheKeyControlURL, url)
    return url, nil
}
```

Rules for `Resolve*()`:

- Cache hit → return cached value.
- Cache miss → do the work, write the result, return it.
- Safe to call concurrently (guard with `sync.Once` / `singleflight` per key when the underlying call is expensive).
- Never depend on another step's `Execute` having already run.

Reference: `mesh/control_url.go:71-83` (`getOrResolveControlURL`).

## Known anti-patterns in this tree (to refactor, not copy)

- `provisioning/create_machine.go:Check` writes `mt.Instance`, `RecordPlanMachineExists`, `RecordPlanMachineHost`. Every downstream step then reads `mt.Instance` and guesses from nil. Move the lookup into `ResolveMachineStatus()` (returns `(exists bool, info *hosting.Instance, err error)`), call it from both `Check` and `Execute`.
- `mesh/install_tailscale.go:Check` writes `RecordPlanMachineMeshIP` to re-seed the cache on idempotent runs. Move the re-seed into the top of `Execute`, or into a `ResolveMeshIP()` helper.
- `mesh/control_url.go:Check` and `mesh/preauth_key.go:Check` only inspect the in-process cache — they do not look at the world. On cold runs they always report "will execute" even for converged clusters. `Check` should call the resolver and compare against desired state, not spy on whether `Execute` has memoized yet.
- `provisioning/wait_cloud_init.go:Check` collapses SSH failures to `false, nil`. Bubble errors up; let the preview render "unknown" rather than "will execute".

## Tests

- Every new `Check` should have a test that calls it twice concurrently with an empty cache and asserts the same verdict both times.
- Table tests should cover: cache hit, cache miss, world-state unreachable (error path).
