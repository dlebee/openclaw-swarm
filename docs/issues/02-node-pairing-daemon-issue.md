# 02 — `openclaw node install` ignores extra environment variables

## Problem

`openclaw node install` creates and enables a systemd user unit for the node
host, but only captures a **hardcoded set** of environment variables into the
unit's `Environment=` directives (see `buildNodeServiceEnvironment` in
`src/daemon/service-env.ts`).

Variables like `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1` — required when the
gateway is LAN-bound (Docker, headscale, VPC) — are **not captured** because
they are not in the allow-list. This is the same class of problem as
[issue 01](./01-insecure-private-ws-not-bootstrapable.md), but on the node
side.

The consequence: after `openclaw node install --force`, the service starts
immediately via `systemctl enable --now`, but the node process cannot connect
to the gateway because it lacks `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`. The
node never registers as a pending device, and pairing fails.

## Current workaround

1. **Install**: run `openclaw node install` normally. The unit is created and
   the service starts — but it cannot reach a LAN-bound gateway.

2. **Post-install**: write a systemd drop-in override at
   `~/.config/systemd/user/openclaw-node.service.d/env.conf` containing
   `Environment=OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`, then **restart** the
   service so the env var is picked up.

3. **Only then** does the node successfully connect and appear as a pending
   device on the gateway, allowing `openclaw devices approve` to complete
   pairing.

This extra restart is annoying — the install should produce a working service
on the first start, not require a post-hoc fixup cycle.

## Ideal fix (openclaw CLI)

Add a repeatable `--daemon-env` flag to `openclaw node install`:

```
openclaw node install --host gw --port 18789 \
  --daemon-env OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1
```

This would merge the extra entries into the `environment` dict returned by
`buildNodeInstallPlan` before the unit is written, so the first start already
has everything it needs.

### openclaw files to change

- `src/cli/node-cli/register.ts` — add `--daemon-env <KEY=VALUE>` option to
  the `install` subcommand
- `src/cli/node-cli/daemon.ts` (`runNodeDaemonInstall`) — pass `--daemon-env`
  entries into `buildNodeInstallPlan`
- `src/commands/node-daemon-install-helpers.ts` (`buildNodeInstallPlan`) —
  merge extra env entries into the `environment` dict
- `src/daemon/service-env.ts` (`buildNodeServiceEnvironment`) — no change
  needed if merge happens at the call-site

### openclaw-swarm2 files affected by the workaround

- `internal/claws/plans/apply/node/bootstrap_node.go` — writes env drop-in
  post-install and restarts
- `internal/claws/plans/apply/node/configure_node.go` — drift-repairs the
  drop-in on subsequent runs
- `internal/platformutil/systemd/systemd.go` — `WriteEnvDropIn` /
  `ReadEnvDropIn`
- `test/infra/stub-systemctl.sh` — `load_unit_env` sources both unit and
  drop-in env for Docker test containers
