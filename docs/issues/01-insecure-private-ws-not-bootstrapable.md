# 01 — OPENCLAW_ALLOW_INSECURE_PRIVATE_WS not bootstrapable

**Upstream issue**: https://github.com/openclaw/openclaw/issues/67565

## Problem

`openclaw onboard --install-daemon` creates and enables a systemd user unit for
the gateway, but provides **no flag** to inject extra `Environment=` lines into
that unit. When the gateway needs `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`
(required for `networking.mode: docker`, `headscale`, or any LAN-bound
gateway), there is no way to set it atomically during bootstrap.

## Current workaround

1. **Bootstrap**: run `openclaw onboard --install-daemon` with the env var
   prefixed on the shell command so the onboard process itself sees it.
   The daemon unit created by `--install-daemon` does **not** include the var.

2. **Post-bootstrap**: the `configure-gateway` step writes a systemd drop-in
   override at `~/.config/systemd/user/openclaw-gateway.service.d/env.conf`
   containing `Environment=OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`, then
   restarts the service.

This means the gateway always requires a restart after initial onboarding
when LAN bind is needed — the first start runs without the env var.

## Ideal fix (openclaw CLI)

Add a repeatable flag to `openclaw onboard`, e.g.:

```
openclaw onboard --install-daemon --daemon-env OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1
```

This would inject the `Environment=` line directly into the generated systemd
unit, eliminating the need for a post-bootstrap drop-in and restart.

The plumbing already exists — `buildGatewayInstallPlan` returns an `environment`
dict that flows through `writeSystemdUnit` → `buildSystemdUnit` →
`Environment=` lines. The missing piece is a CLI flag to inject extra entries.

### openclaw files to change

- `src/cli/program/register.onboard.ts` — add `--daemon-env <KEY=VALUE>` option
- `src/commands/onboard-non-interactive/local/daemon-install.ts` — merge
  `--daemon-env` entries into the `environment` object before `service.install`
- `src/wizard/setup.finalize.ts` — same merge for interactive wizard path
- `src/daemon/systemd-unit.ts` — no change needed (already renders env dict)
- `src/daemon/systemd.ts` — no change needed (already passes env to unit builder)

### openclaw-swarm2 files affected by the workaround

- `internal/claws/plans/apply/gateway/bootstrap_gateway.go` — shell env prefix hack
- `internal/claws/plans/apply/gateway/configure_gateway.go` — drop-in write + restart
- `internal/platformutil/systemd/systemd.go` — `WriteEnvDropIn` / `ReadEnvDropIn`
- `test/infra/stub-systemctl.sh` — `load_dropin_env` for Docker test containers
