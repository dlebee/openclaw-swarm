# Current unsolved problems (fix/cache-problems)

State of the `fix/cache-problems` branch, written after several
destroy → apply → destroy cycles against `manifests/army.yml` (3 Linode VPS:
`gateway-host`, `dev-host-1`, `qa-host-1`, `networking.mode: headscale`).

Phases 1–6 are stable. The apply repeatedly gets stuck in **phase 7/8
(node) → pair-node**. Below is the whole chain of findings so you don't
have to re-derive them.

---

## What's fixed and committed / staged

These are already in the branch (committed on top of `e0e84b9 fix cache
problems`, some still staged locally):

1. **Cache-problems / SSL retry** — host for Linode machines now resolves
   through the plan cache (`ResolveMachineHost`) instead of reading the
   empty manifest `Host`. SSH pool has a liveness probe + retry on
   transient errors (`connection reset by peer`, `exited without exit
   status`, `broken pipe`, `EOF`). Bash-over-SSH calls that matter for
   idempotency use `RunBashWithRetry` / `RunBashOutputWithRetry`.
2. **`claws apply --yes`** — non-interactive apply.
3. **Upstream openclaw bug #04** — `openclaw node install` tries to
   `systemctl --user enable openclaw-gateway.service` on *node* machines.
   Documented in `docs/issues/04-node-install-enables-gateway-unit.md`.
   Workaround: new `node.stub-gateway-unit` step writes a dummy
   `openclaw-gateway.service` user unit before `bootstrap-node` runs so
   the enable call succeeds.
4. **Agent user is the default for post-provisioning** — `common.MachineSSHUser`
   and `automations.DynamicStep.runAs` now cascade
   `SSHUser → AgentUser → "root"`. Provisioning-only steps
   (`create-machine`, `authorize-ssh-key`, `ensure-agent-user`) still pin
   to root via their own `sshLoginUser` helper because the agent user
   doesn't exist yet. Matters because `ensure-agent-user` runs
   `loginctl enable-linger` for the agent user only — systemd `--user`
   services created under root die the moment the SSH session ends.
5. **`XDG_RUNTIME_DIR` in user-systemd scripts** — `bootstrap-gateway`,
   `bootstrap-node`, `stub-gateway-unit` now `export XDG_RUNTIME_DIR=
   /run/user/$(id -u)` before `systemctl --user` / `openclaw node install`
   / `openclaw onboard --install-daemon`. Non-interactive SSH sessions
   don't get this for free, and `systemctl --user` silently refuses
   without it.
6. **`GatewayInternalHost` falls back correctly** — was returning `""`
   when the manifest didn't set `host`/`internal_host`, which made
   `openclaw node install --host ""` default to `--host 127.0.0.1`. The
   unit then loop-crashed with `ECONNREFUSED 127.0.0.1:18789`. Now
   cascades through `InternalHost → mesh IP → Host → plan-cache public IP`.
7. **Mesh IP capture** — `install-tailscale` now records the tailnet IPv4
   in the plan cache via new `scaffold.RecordPlanMachineMeshIP` /
   `LookupPlanMachineMeshIP`. `GatewayInternalHost` prefers this over the
   public IP for reasons explained below.

---

## The remaining problem: pair-node times out

After all the fixes above, the node unit finally gets installed with the
*right* gateway host (gateway's public IP, e.g. `96.126.110.217`). But
the node daemon still can't connect. Journal on `dev-host-1`:

```
node host gateway connect failed: SECURITY ERROR: Cannot connect to
"96.126.110.217" over plaintext ws://. Both credentials and chat data
would be exposed to network interception. Use wss:// for remote URLs.
Safe defaults: keep gateway.bind=loopback and connect via SSH tunnel
(ssh -N -L 18789:127.0.0.1:18789 user@gateway-host), or use Tailscale
Serve/Funnel. Run `openclaw doctor --fix` for guidance.
```

openclaw's node client refuses `ws://` to a **public** IP. The
`OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1` drop-in we write in
`configure-node` doesn't unlock this — the guard is triggered by "is
this IP public?", not by "is insecure allowed?".

### Why passing the Tailscale IP is the intended fix

- Manifest declares `networking.mode: headscale`, which implies "nodes
  talk to gateway over the mesh".
- Mesh (headscale + tailscale) is already set up by phases 3–4.
- Tailscale IPs live in `100.64.0.0/10` (CGNAT / RFC6598) — openclaw's
  "is this private?" check accepts them as private, so the SECURITY
  ERROR goes away without needing TLS.
- The plan-cache plumbing for mesh IPs is in place (item 7 above).

### What might still bite us after that fix

Even with the mesh-IP change, **I haven't been able to observe a green
pair-node end-to-end yet** because the last apply was killed before the
updated binary ran a full node phase. Known remaining hazards:

1. **Gateway's CLI cold start is ~30 s.** `pair-node`'s
   `openclaw devices list --json` polling loop uses 15 attempts × 2 s
   sleep. Each attempt actually takes 30 s because every invocation
   pays a full node+jiti cold start. Worst case: ~8 min per node,
   ~15 min for both. Not broken, just really slow. `NODE_COMPILE_CACHE`
   = `/var/tmp/openclaw-compile-cache` is already set (via
   `StartupOptimEnv`) and populated to 23 MB, but it barely shaves a
   second — cold start is still 29–31 s.
2. **The gateway's `openclaw devices list` was intermittently returning
   `Failed to start CLI: Error: gateway timeout after 10000ms`** during
   debugging. When it finally responded, `pending: []` — meaning the
   node daemon had crashed out before it could register itself. With
   the mesh-IP fix this should go away (node stays up → registers as
   pending → gets approved), but worth watching.
3. **ufw.** We allow `41641/udp` and `in on tailscale0`, but if the
   gateway's `bind: lan` exposes `0.0.0.0:18789`, we haven't explicitly
   opened 18789 on the tailnet interface. Traffic *should* be allowed
   by `ufw allow in on tailscale0`, but it's worth double-checking
   once pair-node gets further.
4. **`ListDevices` / `ApproveDevice` don't set `OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1`**
   for their bash calls. Since they run on the gateway itself and the
   CLI dials `ws://127.0.0.1:18789`, loopback should be fine without
   it — but if the gateway's CLI is unhappy with `bind: lan` while
   dialing loopback, we may need to pass the env var to the bash
   script too.

---

## Reproduction

```
cd /Users/gluwadl/Dev/ai-workspace/openclaw-swarm2
go build -o /tmp/claws ./cmd/cli
cd /Users/gluwadl/Dev/ai-workspace
/tmp/claws destroy -f manifests/army.yml --all --yes
/tmp/claws apply  -f manifests/army.yml --yes
```

Phases 1–6 complete in ~8 min. Phase 7/8 gets to `pair-node` quickly,
but can sit there 8–15 minutes because of openclaw CLI cold-start cost.

---

## Dirty-tree inventory

Not yet committed on `fix/cache-problems`:

```
internal/claws/plans/apply/common/common.go
internal/claws/plans/apply/common/common_test.go
internal/claws/plans/apply/gateway/bootstrap_gateway.go
internal/claws/plans/apply/gateway/gateway.go
internal/claws/plans/apply/mesh/ssh.go
internal/claws/plans/apply/mesh/install_tailscale.go    (mesh IP capture)
internal/claws/plans/apply/node/bootstrap_node.go       (GatewayInternalHost ctx)
internal/claws/plans/apply/node/node.go                 (mesh-IP precedence)
internal/claws/plans/apply/node/stub_gateway_unit.go
internal/claws/plans/apply/security/security_test.go
internal/claws/plans/apply/security/ssh.go
internal/claws/plans/automations/run_step.go
internal/scaffold/plan_cache.go                         (RecordPlanMachineMeshIP)
```

All `go build ./...` green. `go test ./internal/scaffold/...
./internal/claws/plans/apply/{common,security,node,mesh}/...
./internal/claws/plans/automations/...` green.

---

## Open questions for you

- Should the mesh-IP path be unconditional (any `networking.mode` that
  installs tailscale) or only when `NeedsLANBind(gw)` says the gateway
  is LAN-bound?
- Do we want to cut `pair-node`'s polling delay waaay up (e.g. 8
  attempts × 60 s) to stop the retries from racing the node daemon's
  own connect-retry loop?
- Do we want to call `openclaw doctor --fix` on the gateway once after
  pairing is done, or leave that as a manual opt-in?
