# Running integration tests with 5 in parallel

This repo has three integration-test tiers, each behind its own Go build tag.
Inside a tier every test calls `t.Parallel()` (see
`test/integration/*/helpers_parallel_test.go` for the parallel-safety plumbing
— unique manifest prefixes, env-file-scoped fake tokens, etc.), so the tier
will happily run as many tests concurrently as you tell it to.

"5 in parallel" means: **up to 5 test functions execute at the same time
inside a single tier's package**. You get that with Go's `-parallel` flag.

## TL;DR

```bash
cd openclaw-swarm

# Multipass tier — 5 VM-backed tests at once
go test -tags=integration_multipass -parallel 5 -timeout 60m -v \
    ./test/integration/multipass/...

# Docker tier — 5 container-backed tests at once
go test -tags=integration_docker -parallel 5 -timeout 30m -v \
    ./test/integration/docker/...

# Linode tier — 5 cloud-backed tests at once (spends real money)
go test -tags=integration_linode -parallel 3 -timeout 90m -v \
    ./test/integration/linode/...
```

`-parallel 5` is the only flag that changes concurrency. `-v` streams
per-test PASS/FAIL/SKIP lines and each test's `t.Log` / `t.Logf` output as
it happens — without it you get one terminal-wide silence until the whole
batch finishes, which is painful with 5 concurrent VM-backed tests.
Everything else in this doc is context.

## What `-parallel` does and doesn't do

- `-parallel N` caps the number of `t.Parallel()`-marked tests that run
  concurrently **inside a single test binary**. Default is `GOMAXPROCS`, which
  on a modern laptop is 8–12 — higher than you probably want when each test
  boots VMs.
- `-p N` caps the number of **test binaries** (i.e. packages) built and run
  at once. Each tier lives in a single package (`./test/integration/multipass`,
  etc.), so `-p` is not what you want.
- Skipped preflight tests (missing `multipass` CLI, missing `LINODE_TOKEN`)
  don't consume a parallel slot — they return before calling `t.Parallel()`.

So: **one tier, one package, `-parallel 5`**. That's the knob.

## Pick the right tier

| Tier | Build tag | Backs tests with | Needs |
| ---- | --------- | ---------------- | ----- |
| `docker` | `integration_docker` | oc-test containers (stubbed systemd) | Docker daemon |
| `multipass` | `integration_multipass` | real Ubuntu VMs on your machine | `multipass` CLI on PATH |
| `linode` | `integration_linode` | real Linode instances | `LINODE_TOKEN` env or `manifests/.env` |

See `test/integration/*/doc.go` for what each tier does and doesn't cover.

## Multipass: the common case

The multipass tier is the one most devs run locally with parallelism. Every
test in `test/integration/multipass/*_test.go` calls `t.Parallel()` near the
top (see e.g. `multipass_gateway_test.go:71`, `multipass_node_test.go:99`,
`multipass_cron_test.go:165`).

```bash
cd openclaw-swarm
go test -tags=integration_multipass -parallel 5 -timeout 60m -v \
    ./test/integration/multipass/...
```

### Sizing notes for `-parallel 5`

- Each test launches 1–3 VMs via `multipass launch`. With 5 tests in flight
  that's roughly **10–15 Ubuntu 24.04 VMs at once**.
- Default per-VM footprint is ~1 CPU, 1 GiB RAM, 5 GiB disk. At 15 VMs plan
  for **~16 GiB RAM and ~75 GiB free disk**.
- `multipass launch` is serialized by a file lock
  (`acquireLaunchLock` in `internal/hosting/multipass/provider.go`) to dodge
  a multipassd DHCP race — so launches happen one at a time even with
  `-parallel 5`, but everything after launch (cloud-init, apply phases,
  teardown) runs concurrently.
- On a 2023-era MacBook Pro M2, 5-way is comfortable. On an older laptop,
  drop to `-parallel 3`.

### Before you run

```bash
# Sanity check
multipass version
multipass list

# Optional: nuke any leftovers from a previous aborted run
multipass delete --all --purge
```

Note: `multipass delete --all --purge` is **not scoped to this project** —
it deletes every multipass VM on the host. The scoped equivalent is
`claws -f <manifest> destroy --all --yes`, which only targets VMs tagged
`claws/<prefix>`.

### After a failed run

Each test registers a `t.Cleanup` that runs `DeleteInstance` per VM, and
the tier's helpers also rely on randomized manifest prefixes
(`m.Prefix = "it-<name>-<rand>"`) so leftovers from one run never collide
with another. If a run is `SIGKILL`'d mid-flight, clean up manually:

```bash
multipass list                  # look for it-* VMs
multipass delete --all --purge  # or delete specific it-* ones
rm -rf ~/.cache/openclaw/multipass/tags/*.json
```

## Docker: fast tier

No VMs, just containers. `-parallel 5` here is cheap — memory and disk are
negligible, the limit is Docker daemon throughput.

```bash
cd openclaw-swarm
go test -tags=integration_docker -parallel 5 -timeout 30m -v \
    ./test/integration/docker/...
```

## Linode: cloud tier (real money)

```bash
cd openclaw-swarm
export LINODE_TOKEN=...   # or drop LINODE_TOKEN=... into manifests/.env
go test -tags=integration_linode -parallel 5 -timeout 90m -v \
    ./test/integration/linode/...
```

Read `test/integration/linode/doc.go` first — it lists per-test cost
envelopes. Five parallel tests means five times the cost; the harness always
registers cleanup hooks before creating instances, so a panic or `-timeout`
hit still tears VMs down.

## Useful flags

```bash
# Run only one specific test at -parallel 5 (it still benefits from parallel
# sub-tests if the test uses t.Run + t.Parallel internally):
go test -tags=integration_multipass -parallel 5 -timeout 60m -v \
    -run TestCronAgentWithNodeExec \
    ./test/integration/multipass/...

# Keep going after the first failure instead of fail-fast:
go test -tags=integration_multipass -parallel 5 -timeout 60m -v \
    -failfast=false ./test/integration/multipass/...

# Stream logs live instead of buffering per-test:
go test -tags=integration_multipass -parallel 5 -timeout 60m -v \
    -count=1 ./test/integration/multipass/...

# Disable Go's test cache (integration tests should never be cached anyway,
# but -count=1 is the idiomatic way to force a re-run):
go test -tags=integration_multipass -parallel 5 -count=1 -timeout 60m \
    ./test/integration/multipass/...
```

## Why `-timeout` is not optional here

Go's default is 10 minutes per `go test` invocation (total, across all
tests). A single multipass apply easily burns that on a cold image cache.
With `-parallel 5` the wall-clock time of the slowest 5-test batch is what
matters, but the total-wall-time budget still applies. `60m` is a safe
default for the multipass tier; bump it if you see `panic: test timed out`.

## Don't do this

- **Don't** run two tiers at once in the same `go test` call. Build tags are
  mutually exclusive in practice and you'll just skip one tier silently.
- **Don't** set `-parallel` higher than you have resources for. A swap-storm
  on multipassd corrupts DHCP leases and produces spectacularly confusing
  "wrong host key" SSH failures.
- **Don't** rely on `multipass delete --all --purge` in a script that runs
  alongside your own unrelated VMs. Use `claws -f <manifest> destroy --all
  --yes` for scoped teardown.
