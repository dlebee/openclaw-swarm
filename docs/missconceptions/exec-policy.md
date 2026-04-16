# Misconceptions Around Exec-Policy, Exec-Approvals, and Agent Execution

**Date:** 2026-04-16
**Status:** Resolved — swarm2 does NOT need `exec-approvals.json` manipulation.

---

## Background

In `openclaw-swarm` (v1), getting an agent to execute shell commands on a node
required configuring **three independent layers**, each on a different machine,
using different mechanisms. The v1 codebase treated them as one blurred concept,
which led to a series of misunderstandings about what was actually needed and
where.

This document captures what each layer does, the misconceptions v1 introduced,
what we discovered during swarm2 integration testing, and why the cleaner
architecture eliminates an entire layer of raw JSON manipulation.

---

## The Three Layers

### Layer 1: Agent exec config (gateway — `openclaw.json`)

**Where:** Gateway machine, inside `~/.openclaw/openclaw.json`.
**What it controls:** Which agent runs commands on which target.

Each agent in `agents.list[]` has a `tools.exec` block:

```json
{
  "id": "scraper",
  "tools": {
    "exec": {
      "host": "node",
      "node": "scraper-node",
      "security": "full"
    }
  }
}
```

| Field      | Meaning |
|------------|---------|
| `host`     | `"gateway"` = execute locally on the gateway machine. `"node"` = dispatch to a paired node over WebSocket. |
| `node`     | Name of the target node (only when `host: node`). Must match a paired device's `displayName`. |
| `security` | The per-agent security level stored in the gateway config. Controls what the gateway *offers* to the node. |

**How it's set in swarm2:**

```
openclaw config set --batch-json '[
  {"path":"agents.list[0].tools.exec.host","value":"node"},
  {"path":"agents.list[0].tools.exec.node","value":"scraper-node"},
  {"path":"agents.list[0].tools.exec.security","value":"full"}
]'
```

Implemented in `internal/claws/plans/apply/agents/configure_tools.go` →
`ConfigureToolsStep`. Clean CLI, no raw JSON writes.

---

### Layer 2: Node exec-policy (node machine — `openclaw exec-policy set`)

**Where:** Node machine, stored in `~/.openclaw/exec-approvals.json` under
`defaults.security` and `defaults.ask`.
**What it controls:** Whether the node process accepts incoming exec commands
*at all*, regardless of what the gateway says.

```json
{
  "defaults": {
    "security": "full",
    "ask": "off"
  }
}
```

| Field      | Values | Meaning |
|------------|--------|---------|
| `security` | `deny` / `allowlist` / `full` | `deny` = reject all exec. `allowlist` = only pre-approved commands. `full` = accept everything. |
| `ask`      | `off` / `on-miss` / `always` | `off` = never prompt for confirmation. `on-miss` = prompt if command not in allowlist. `always` = prompt every time. |

This is the **node-side gate**. If `security: deny`, the node will refuse every
exec tool call the gateway dispatches to it — even if the gateway config says
`security: full` for that agent.

**How it's set in swarm2:**

```
openclaw exec-policy set --security full --ask off
```

Implemented in `internal/claws/plans/apply/node/exec_policy.go` →
`ExecPolicyStep`. Clean CLI, no raw JSON writes.

---

### Layer 3: Gateway exec-approvals (gateway — `exec-approvals.json`)

**Where:** Gateway machine, `~/.openclaw/exec-approvals.json`.
**What it controls:** Per-agent security *overrides* on the gateway side.

```json
{
  "agents": {
    "scraper": {
      "security": "full"
    }
  }
}
```

This file is **separate** from `openclaw.json`. In v1, there was no
`openclaw config set` path that reached it. v1 wrote this file using raw `node
-e` scripts that read/parse/modify/write JSON — the exact kind of fragile
direct manipulation that swarm2 was designed to eliminate.

**How it was set in v1:**

```javascript
// v1: raw JSON write via node -e (agents.go → upsertExecApprovalsSecurity)
const fs = require("fs"), os = require("os");
const p = os.homedir() + "/.openclaw/exec-approvals.json";
let f = {};
try { f = JSON.parse(fs.readFileSync(p, "utf8")); } catch(e) {}
if (!f.agents) f.agents = {};
if (!f.agents["scraper"]) f.agents["scraper"] = {};
f.agents["scraper"].security = "full";
fs.writeFileSync(p, JSON.stringify(f, null, 2));
```

**How it's set in swarm2:** It isn't. We don't touch this file. See below.

---

## The v1 Misconceptions

### Misconception 1: "exec-approvals.json on the gateway is required"

v1 always wrote `exec-approvals.json` on the gateway for every agent that had
`tools.exec.security` set. This was documented as a necessary step. The function
`upsertExecApprovalsSecurity` was called during every agent deploy.

**Reality:** The gateway's `exec-approvals.json` is a per-agent *override*. If
the agent's `tools.exec.security` is already set correctly in `openclaw.json`
(via `agents.list[N].tools.exec.security`), the override file is redundant. The
gateway reads `security` from the agent config in `openclaw.json` first. The
`exec-approvals.json` file only matters if you want to override the config value
for a specific agent without changing `openclaw.json`.

### Misconception 2: "The gateway pushes exec-policy to nodes"

v1 code comments suggested a relationship between the gateway's
exec-approvals.json and the node's exec-approvals.json. In reality, these are
completely independent files on completely independent machines:

| File | Machine | Managed by | Purpose |
|------|---------|------------|---------|
| `~/.openclaw/exec-approvals.json` | **Gateway** | `openclaw config set` (agents config) | Per-agent security in gateway context |
| `~/.openclaw/exec-approvals.json` | **Node** | `openclaw exec-policy set` | Node-wide default policy |

The gateway does **not** sync, push, or propagate anything to nodes. Each
machine enforces its own policy independently. If a node has `security: deny`,
it will reject commands even if the gateway's config says `security: full`.

### Misconception 3: "There's no CLI command for exec-policy"

v1 initially manipulated `exec-approvals.json` on nodes with raw JSON writes
(see `upsertNodeExecApprovalsDefaults` in v1's `node.go`). The v1 migration
later discovered that `openclaw exec-policy set` exists and works cleanly:

```bash
openclaw exec-policy set --security full --ask off
```

This has been available since at least openclaw 2026.4.x. It updates
`exec-approvals.json` on the local machine properly.

### Misconception 4: "Both gateway and node exec-approvals must be set for exec to work"

The end-to-end integration test in swarm2 proves this wrong. The test:

1. Sets agent exec config in `openclaw.json` (Layer 1)
2. Sets node exec-policy via `openclaw exec-policy set` (Layer 2)
3. Does NOT write `exec-approvals.json` on the gateway (Layer 3 skipped)
4. Dispatches `system.which` from gateway to node via `openclaw nodes invoke`
5. **Command succeeds.** The node returns the result.

The test explicitly checks for `exec-approvals.json` on the gateway and confirms
it does not exist:

```
exec-approvals.json state:
    (not found)
```

Yet the full exec pipeline works. Layer 3 is not needed.

---

## What swarm2 Actually Does

The swarm2 apply pipeline configures execution in two clean steps, both using
official CLI commands:

### Step 1: Node phase — `ExecPolicyStep`

**File:** `internal/claws/plans/apply/node/exec_policy.go`

Runs on the **node machine** via SSH. Reads current policy with
`openclaw config get tools.exec.security` and `tools.exec.ask`, compares
against the manifest's `nodes[].exec_policy`, and calls
`openclaw exec-policy set` if there's drift.

Manifest:

```yaml
nodes:
  - name: scraper-node
    gateway: gateway
    reference: scraper-host
    exec_policy:
      security: full
      ask: "off"
```

This writes to `~/.openclaw/exec-approvals.json` on the node via the proper CLI
command. The step is idempotent — it only writes when the current value differs
from the desired value.

### Step 2: Agents phase — `ConfigureToolsStep`

**File:** `internal/claws/plans/apply/agents/configure_tools.go`

Runs on the **gateway machine** via SSH. Sets per-agent tools config in
`openclaw.json` using `openclaw config set --batch-json`. This includes
`tools.exec.host`, `tools.exec.node`, and `tools.exec.security`.

Manifest:

```yaml
agents:
  - id: scraper
    tools:
      exec: { host: node, node: scraper-node, security: full }
```

This writes to `~/.openclaw/openclaw.json` on the gateway. No separate
`exec-approvals.json` file is touched.

---

## The Node Restart After Pairing — A Related Discovery

During integration testing we hit an additional issue: the end-to-end exec test
timed out because the node's WebSocket connection to the gateway never
established, even though pairing was approved.

### Root cause

The apply pipeline runs steps in order:

1. `bootstrap-node` — installs openclaw, writes systemd unit, starts daemon
2. `configure-node` — writes env drop-in, restarts daemon
3. `exec-policy` — sets security/ask policy
4. `pair-node` — approves device on gateway

When the node daemon starts (step 1–2), it immediately connects to the gateway
via WebSocket. The gateway rejects it with `pairing required` because the device
hasn't been approved yet. The daemon exits.

After step 4 approves the pairing, there is nothing to restart the daemon. On
real systemd, `Restart=always` would automatically restart the crashed process.
But in Docker test containers (where we use a stub systemctl), there is no
automatic restart.

### Fix

`PairNodeStep.Execute()` now SSHes back to the node machine after approving the
device on the gateway and explicitly restarts the `openclaw-node` service:

```go
// pair_node.go — after approval
nodeClient, nodeKey, err := common.BorrowSSH(ctx, s.dial, ...)
if err != nil {
    return fmt.Errorf("pair-node: dial node for restart: %w", err)
}
defer common.ReturnSSH(ctx, nodeKey, nodeClient)

if err := systemd.Restart(nodeClient, nodeUnit, true); err != nil {
    return fmt.Errorf("pair-node: restart node daemon: %w", err)
}
```

This is also the correct behavior for real deployments — an explicit restart
after pairing is more reliable than depending on systemd's `Restart=always`
retry loop, which might have an exponential backoff delay.

### Stub systemctl fix

The Docker test stub (`test/infra/stub-systemctl.sh`) also had a bug: `pkill -f
"openclaw node"` didn't match the actual command line
(`/usr/bin/node .../openclaw/dist/index.js node run`) because there's no
contiguous "openclaw node" substring — there's `openclaw/dist/index.js node`
with a `/` in between. Fixed to use `pkill -f "openclaw/dist/index.js node"`.

---

## Integration Test Coverage

`TestApplyExecute` in `test/integration/integration_test.go` verifies the
complete exec pipeline:

| Assertion | What it checks |
|-----------|---------------|
| 10 | Node `tools.exec.security=full` (exec-policy applied) |
| 10b | Node `tools.exec.ask=off` (exec-policy applied) |
| 17 | Agent `tools.exec.security` present in `openclaw.json` on gateway |
| 18 | `exec-approvals.json` does NOT exist on gateway (confirming it's not needed) |
| 19 | `openclaw nodes invoke --node scraper-node --command system.which` succeeds end-to-end |

Assertion 19 is the definitive proof: the gateway dispatches `system.which` to
the scraper-node over WebSocket, the node executes it and returns the result
containing `echo`. This works with only Layer 1 (agent config) and Layer 2
(node exec-policy) — no Layer 3 (gateway exec-approvals.json).

---

## Summary

| Concern | v1 approach | swarm2 approach | Needed? |
|---------|-------------|-----------------|---------|
| Agent exec routing | `openclaw config set` (clean) | `openclaw config set --batch-json` (clean) | Yes |
| Node exec-policy | Raw JSON → later `openclaw exec-policy set` | `openclaw exec-policy set` from the start | Yes |
| Gateway exec-approvals.json | Raw `node -e` JSON manipulation | Not used | **No** |

The gateway's `exec-approvals.json` is a legacy override mechanism. When the
agent's `tools.exec.security` is set properly in `openclaw.json`, the override
file is redundant. swarm2 proves this with a passing end-to-end integration test
that has no `exec-approvals.json` on the gateway at all.
