# 01 — OPENCLAW_ALLOW_INSECURE_PRIVATE_WS not bootstrapable

**Upstream issue**: https://github.com/openclaw/openclaw/issues/67565
**Upstream PR**: https://github.com/openclaw/openclaw/pull/68240 (`--daemon-env` for `openclaw onboard`)
**Status**: ✅ **Resolved for openclaw-swarm** — no longer needed for this specific env var.

## Original Problem

`openclaw onboard --install-daemon` (and `openclaw node install`) create systemd
units but originally provided **no way** to inject extra `Environment=` lines.
When the service needs `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1` (required for
`networking.mode: docker`, `headscale`, or any LAN-bound gateway/node), there
was no way to set it atomically during bootstrap.

## Resolution

**`openclaw node install` now captures `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS`
from the install environment** and bakes it into the systemd unit's
`Environment=` line automatically. When bootstrap runs with the env var
prefixed (e.g. `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 openclaw node install ...`),
the generated unit includes it — no drop-in override needed.

Fixed in openclaw-swarm: [`81c6ddb`](https://github.com/dlebee/openclaw-swarm/commit/81c6ddb) —
removed `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS` from the drop-in env and the
configure step. The bootstrap `envPrefix` is sufficient.

## Remaining upstream item

PR [#68240](https://github.com/openclaw/openclaw/pull/68240) adds a general
`--daemon-env KEY=VALUE` flag to `openclaw onboard --install-daemon`. This is
still useful for **other** env vars that need to be injected into the gateway
unit (not node), but is no longer needed for `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS`
specifically.

**As of OpenClaw 2026.4.9**: `--daemon-env` is not yet shipped. The PR is open.

## What changed in openclaw-swarm

- `bootstrap_node.go` — sets `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1` via
  `envPrefix` before `openclaw node install`; upstream captures it into the unit
- `configure_node.go` — `nodeEnv()` no longer includes
  `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS`; only startup optimisation vars remain
- The drop-in write + restart cycle for this specific var is eliminated
