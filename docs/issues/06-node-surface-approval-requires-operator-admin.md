# 06 — `openclaw nodes approve` requires `operator.admin` scope, blocking automation

## Problem

In OpenClaw >= 2026.5.18 (introduced by gateway commit `6160e7a411`
"fix(gateway): hide unapproved node surfaces"), device pairing and node-surface
approval are two separate steps:

1. **Device pairing** — `openclaw devices approve <requestId>` — requires
   `operator.pairing` scope. Works fine in automation.
2. **Surface approval** — `openclaw nodes approve <requestId>` — requires
   `operator.admin` scope. **Blocked in automation.**

After device pairing, a node registers a pending surface request in
`~/.openclaw/nodes/pending.json`. Until that request is approved the node's
`effectiveCommands` is empty, so agents dispatching `system.run` over the node
websocket (cron exec, node-exec path) fail with
`exec host=node requires a node that supports system.run`.

The `operator.admin` scope requirement is the blocker. The gateway machine's
local CLI device is paired with `operator.pairing` scope (the scope
`openclaw cron list` requests when triggering the initial pairing in
`pair-gateway-device`). Passing `--token <gateway.auth.token>` doesn't help
either — `gateway.auth.token` is a WebSocket admission key with no operator
scopes at all. Both paths hit:

```
GatewayClientRequestError: missing scope: operator.admin
```

## Why this scope requirement is inconsistent

Device pairing (`operator.pairing`) is the higher-trust step: it grants a
machine ongoing access to the gateway WebSocket. Surface approval is a
follow-up confirmation of *what that already-trusted machine can do*. Requiring
a higher scope (`operator.admin`) for the lower-trust follow-up than for the
initial trust grant is backwards.

If an operator has sufficient trust to approve a device, they have sufficient
trust to approve its surface.

## Current workaround in openclaw-swarm

`pair-node` (>= 2026.5.18 code path in `pair_node_surface.go`) bypasses the
CLI entirely and directly edits the gateway's on-disk state:

1. Reads the pending entry from `~/.openclaw/nodes/pending.json`.
2. Writes it into `~/.openclaw/nodes/paired.json` (synthesizing a token the
   same way the RPC handler would).
3. Removes the entry from `pending.json`.
4. Restarts `openclaw-gateway` so it reloads the state.
5. Restarts `openclaw-node` so it reconnects and triggers
   `reconcileNodePairingOnConnect`, which populates `effectiveCommands`.

This mirrors the workaround `pair-gateway-device` already uses to escalate the
CLI device's scope in `devices/paired.json` — the pattern is consistent within
the codebase but is not a proper solution.

## Desired fix (openclaw CLI / gateway)

Either of the following would eliminate the workaround:

### Option A — Lower the scope requirement (preferred)

Change the `node.pair.approve` RPC handler to require `operator.pairing`
instead of `operator.admin`. An operator who can approve devices can approve
surfaces.

**openclaw files to change:**

- `src/gateway/rpc/node-pair-approve.ts` (or wherever `node.pair.approve` is
  registered) — change the required scope from `operator.admin` to
  `operator.pairing`.

### Option B — Bootstrap CLI device with `operator.admin` scope

Ensure the CLI device created during `openclaw onboard` (or the first
`openclaw devices approve`) is granted `operator.admin` scope, so all
subsequent CLI commands on the gateway machine run with admin capability.

**openclaw files to change:**

- `src/commands/devices-approve.ts` or `src/gateway/device-registry.ts` —
  grant `operator.admin` to the first CLI device approved after `onboard`.

### Option C — Add an `--admin-token` flag to `openclaw nodes approve`

Allow passing the gateway's own bootstrap token with elevated privilege,
similar to how some approve commands already accept `--token`.

## Impact

Any automation tool or CI pipeline that:
- Uses OpenClaw >= 2026.5.18, and
- Needs agents to dispatch `system.run` (or any node command) via the node
  websocket (cron exec, node-exec path)

...must either implement the file-surgery workaround or run a version of
OpenClaw older than 2026.5.18.

## openclaw-swarm tracking

- **Workaround implemented in:** `internal/claws/plans/apply/node/pair_node_surface.go`
  — function `promotePendingNodeToPaired`
- **Version gate:** `NodeSurfaceApprovalRequiredSince = 2026.5.18`
- **Tests covering this path:** `TestCronAgentWithNodeExec`, `TestNodeSmoke`
  in `test/integration/multipass/`
