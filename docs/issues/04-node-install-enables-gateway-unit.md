# 04 — `openclaw node install` tries to enable the **gateway** systemd unit on a node host

## Symptom

On a freshly provisioned node VM (no gateway service installed), running
`openclaw node install --host <gw> --port 18789 --runtime node --force`
fails with:

```
Node install failed: Error: systemctl enable failed: Failed to enable unit:
Unit file openclaw-gateway.service does not exist.
```

The install is transactional, so the unit file that was just written
(`~/.config/systemd/user/openclaw-node.service`) and `~/.openclaw/node.json`
are rolled back. Nothing is left behind; re-running produces the same error.

In `openclaw-swarm2` this surfaces as a hard failure of the `bootstrap-node`
step in phase 7 (`node`), after phases 1–6 (provisioning → channels) have all
completed successfully:

```
[1] dev-node-1 > bootstrap-node: error bootstrap-node: openclaw node install failed:
    Process exited with status 1: Node install failed: Error: systemctl enable failed:
    Failed to enable unit: Unit file openclaw-gateway.service does not exist.
```

## Why containers don't see this

The integration-test containers ship a stub `systemctl` at
`test/infra/stub-systemctl.sh` (installed over `/usr/local/bin/systemctl`
in `test/infra/Dockerfile`). The stub accepts every verb/unit combination
and returns 0, including `systemctl --user enable openclaw-gateway.service`
on a node-only image. That masks the bug in CI.

On a real VPS there is no stub, so real systemd rejects the enable and the
installer unwinds.

## Root cause (in `openclaw`)

The node install path correctly sets `OPENCLAW_SYSTEMD_UNIT=openclaw-node`
on the service env (see `src/daemon/node-service.ts`):

```ts
// openclaw/src/daemon/node-service.ts:15-21
return {
  ...env,
  OPENCLAW_LAUNCHD_LABEL: resolveNodeLaunchAgentLabel(),
  OPENCLAW_SYSTEMD_UNIT: resolveNodeSystemdServiceName(),
  OPENCLAW_WINDOWS_TASK_NAME: resolveNodeWindowsTaskName(),
  OPENCLAW_TASK_SCRIPT_NAME: NODE_WINDOWS_TASK_SCRIPT_NAME,
  OPENCLAW_LOG_PREFIX: "node",
};
```

`resolveSystemdServiceName(env)` honours that override:

```ts
// openclaw/src/daemon/systemd.ts:52-58
function resolveSystemdServiceName(env: GatewayServiceEnv): string {
  const override = env.OPENCLAW_SYSTEMD_UNIT?.trim();
  if (override) {
    return override.endsWith(".service") ? override.slice(0, -".service".length) : override;
  }
  return resolveGatewaySystemdServiceName(env.OPENCLAW_PROFILE);
}
```

The unit file is written to the right path via this resolver. But the
activation step immediately afterward bypasses it and hardcodes the
gateway resolver:

```ts
// openclaw/src/daemon/systemd.ts:541-552
async function activateSystemdService(params: { env: GatewayServiceEnv }) {
  const serviceName = resolveGatewaySystemdServiceName(params.env.OPENCLAW_PROFILE);
  const unitName = `${serviceName}.service`;
  const reload = await execSystemctlUser(params.env, ["daemon-reload"]);
  if (reload.code !== 0) {
    throw new Error(`systemctl daemon-reload failed: ${reload.stderr || reload.stdout}`.trim());
  }

  const enable = await execSystemctlUser(params.env, ["enable", unitName]);
  if (enable.code !== 0) {
    throw new Error(`systemctl enable failed: ${enable.stderr || enable.stdout}`.trim());
  }
  // ...
}
```

So the sequence on a node is:

1. `writeSystemdUnit` writes `~/.config/systemd/user/openclaw-node.service` ✔
2. `activateSystemdService` runs `systemctl --user enable openclaw-gateway.service` ✗
3. systemd: `Unit file openclaw-gateway.service does not exist` → throws
4. `installSystemdService` unwinds → node.json never persisted

`uninstallSystemdService` has the same mistake at `systemd.ts:591` — it also
resolves to `openclaw-gateway` regardless of `OPENCLAW_SYSTEMD_UNIT`.

## Ideal fix (in `openclaw`)

Two-line patch in `openclaw/src/daemon/systemd.ts`:

```diff
 async function activateSystemdService(params: { env: GatewayServiceEnv }) {
-  const serviceName = resolveGatewaySystemdServiceName(params.env.OPENCLAW_PROFILE);
+  const serviceName = resolveSystemdServiceName(params.env);
   const unitName = `${serviceName}.service`;
   ...
 }

 export async function uninstallSystemdService({ env, stdout }: GatewayServiceManageArgs) {
   await assertSystemdAvailable(env);
-  const serviceName = resolveGatewaySystemdServiceName(env.OPENCLAW_PROFILE);
+  const serviceName = resolveSystemdServiceName(env);
   const unitName = `${serviceName}.service`;
   ...
 }
```

Both activation and uninstallation already have the right resolver
(`resolveSystemdServiceName`) next to them — they just need to be wired to
it. Once shipped, bump the pinned version in
`internal/claws/plans/apply/common/install_openclaw.go` and any workaround
can be removed.

## Short-term workaround (in `openclaw-swarm2`)

Before calling `openclaw node install`, pre-create a no-op
`openclaw-gateway.service` user unit so the broken `enable` call
succeeds; clean it up after install commits.

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/openclaw-gateway.service <<'EOF'
[Unit]
Description=placeholder (workaround for openclaw node install bug, issue #04)
[Service]
Type=oneshot
ExecStart=/bin/true
RemainAfterExit=yes
[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload

openclaw node install --host "$GW" --port 18789 --display-name "$NAME" --runtime node --force

systemctl --user disable openclaw-gateway.service || true
rm -f ~/.config/systemd/user/openclaw-gateway.service
systemctl --user daemon-reload
```

Call-site to change: `internal/claws/plans/apply/node/bootstrap_node.go`,
the `script` block in `Execute()` around line 102-106 (wrap pre-stub
before the `openclaw node install` line, cleanup after).

### Caveats

- `openclaw node install` uses `systemctl --user`, which requires a user
  session. On a fresh VPS running as `root` (or as the agent user) you
  still need `loginctl enable-linger <user>` for the user bus to exist
  outside an SSH session. That's orthogonal to this bug but trips the same
  command.
- `Verify()` in `bootstrap_node.go` greps `openclaw-node.service`, which
  the installer does write before the broken activation, so no change is
  needed there once the install commits.

## Reproduction

1. `claws destroy -f manifests/army.yml --all --yes`
2. `claws apply -f manifests/army.yml --yes`
3. Observe phases 1–6 succeed, then `bootstrap-node` on every node host
   errors with `openclaw-gateway.service does not exist`.

Confirmed on run 5 of branch `fix/cache-problems`
(`/tmp/apply-run-5.log`).
