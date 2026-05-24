# 03 — Agent phase concurrency limited to 1 (config file race)

**Status: RESOLVED** — OpenClaw 2026.5.x implemented file locking (Option B)
around config reads/writes. The swarm tool now uses `maxAgentConcurrency = 5`.

## Problem (historical)

Multiple agents can target the same gateway machine. Each step in the agents
phase (`add-agent`, `ensure-model`, `configure-tools`, etc.) reads and writes
`~/.openclaw/openclaw.json` on the gateway via `openclaw agents add`,
`openclaw config set --batch-json`, and similar CLI commands.

When two agent targets run **concurrently** on the same gateway, they race on
the shared config file:

1. Agent A reads `openclaw.json`, adds itself, writes the file back.
2. Agent B reads the **same** pre-A version, adds itself, writes back —
   **overwriting Agent A's entry**.
3. Agent A's `Verify` step calls `openclaw agents list --json` and can't find
   itself → step fails.

This was observed in integration tests where `assistant` and `scraper` both
target the single `gateway` machine. With concurrency > 1, one agent's add
consistently disappeared.

## Current workaround

`maxAgentConcurrency` is set to **1** in
`internal/claws/plans/apply/agents/agents.go`, serializing all agent targets.
This is correct but slow for manifests with many agents spread across
**different** gateways (which wouldn't actually race).

## Possible improvements

### Option A — Per-gateway serialization

Group agent targets by their resolved `Machine.Host` (or gateway name) and run
groups in parallel but serialize targets within each group. This is the
highest-value improvement since it removes the bottleneck for multi-gateway
deployments while preserving correctness.

Implementation sketch:
- Build a map `gateway -> []scaffold.Target`.
- For each gateway, create a sub-phase or use the scaffold's cell-level
  locking to serialize targets that share a host.

### Option B — Upstream file locking in openclaw CLI

If `openclaw config set` and `openclaw agents add` used advisory file locks
(e.g. `flock`) around config reads/writes, concurrent invocations on the same
host would naturally serialize. This would fix the problem at the source and
allow the swarm tool to use higher concurrency safely.

### Option C — Retry with conflict detection

After each `Execute`, re-verify and retry if the agent is missing. This papers
over the race but doesn't prevent it — repeated retries under high concurrency
would still be unreliable.

## Files affected

- `internal/claws/plans/apply/agents/agents.go` — `maxAgentConcurrency = 5` (was 1)

## Resolution

OpenClaw 2026.5.x implemented file locking in the CLI (Option B), so concurrent
`openclaw agents add` and `openclaw config set` commands now serialize properly.
The swarm tool has raised `maxAgentConcurrency` from 1 to 5 to take advantage
of this fix.
