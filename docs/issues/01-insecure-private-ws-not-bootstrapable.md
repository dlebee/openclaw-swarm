# 01 — OPENCLAW_ALLOW_INSECURE_PRIVATE_WS not bootstrapable

**Upstream issue**: https://github.com/openclaw/openclaw/issues/67565
**Upstream PR**: https://github.com/openclaw/openclaw/pull/68240
**Status**: ⏳ **Patch in progress** — PR #68240 adds `--daemon-env` flag but is not yet merged. Issue #67565 closed as superseded by the PR.

## Problem

`openclaw onboard --install-daemon` creates and enables a systemd user unit for
the gateway, but provides **no flag** to inject extra `Environment=` lines into
that unit. When the gateway needs `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`
(required for `networking.mode: docker`, `headscale`, or any LAN-bound
gateway), there is no way to set it atomically during bootstrap.

## Upstream fix (PR #68240 — not yet merged)

PR [#68240](https://github.com/openclaw/openclaw/pull/68240) adds a repeatable
`--daemon-env KEY=VALUE` flag to `openclaw onboard`:

```
openclaw onboard --install-daemon --daemon-env OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1
```

The PR modifies:
- `src/cli/program/register.onboard.ts` — adds `--daemon-env <KEY=VALUE>` option
- `src/commands/onboard-non-interactive/local/daemon-install.ts` — parses and merges
  `--daemon-env` entries into the environment object before `service.install`
- `src/wizard/setup.finalize.ts` — same merge for interactive wizard path

No changes needed in `systemd-unit.ts` or `systemd.ts` (plumbing already
supports arbitrary `Environment=` lines).

**As of OpenClaw 2026.4.9**: `--daemon-env` is **not yet available**. The PR is
open and awaiting review/merge.

## Current workaround (still required)

Until PR #68240 lands, the two-phase workaround remains necessary:

1. **Bootstrap**: run `openclaw onboard --install-daemon` with the env var
   prefixed on the shell command so the onboard process itself sees it.
   The daemon unit created by `--install-daemon` does **not** include the var.

2. **Post-bootstrap**: the `configure-gateway` step writes a systemd drop-in
   override at `~/.config/systemd/user/openclaw-gateway.service.d/env.conf`
   containing `Environment=OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`, then
   restarts the service.

This means the gateway always requires a restart after initial onboarding
when LAN bind is needed — the first start runs without the env var.

### openclaw-swarm files affected by the workaround

- `internal/claws/plans/apply/gateway/bootstrap_gateway.go` — shell env prefix hack
- `internal/claws/plans/apply/gateway/configure_gateway.go` — drop-in write + restart
- `internal/platformutil/systemd/systemd.go` — `WriteEnvDropIn` / `ReadEnvDropIn`
- `test/infra/stub-systemctl.sh` — `load_dropin_env` for Docker test containers

## When the fix lands

Once `--daemon-env` ships in a release:
1. Update `bootstrap_gateway.go` to pass `--daemon-env OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`
2. Remove the drop-in write from `configure_gateway.go`
3. Remove `WriteEnvDropIn` / `ReadEnvDropIn` from `systemd.go` (unless used elsewhere)
4. Simplify `stub-systemctl.sh` (no more `load_dropin_env`)
