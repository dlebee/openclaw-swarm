# Migration: what's left from openclaw-swarm v1

Tracking parity between `openclaw-swarm` (v1) and `openclaw-swarm2`.
Items are grouped by area; checked items are implemented in swarm2.

## Apply phases

- [x] Provisioning (create Linode machines, authorize SSH keys, ensure agent user)
- [x] Security (ufw, fail2ban, unattended-upgrades, security packages)
- [ ] Mesh — headscale install, tailscale join, Caddy HTTPS reverse proxy, preauth key management
- [x] Gateway (install nodejs/openclaw, bootstrap, configure, pair device)
- [x] Node (install nodejs/openclaw, bootstrap, configure, pair)
- [x] Agents (add, ensure-model, configure-workspace, configure-tools, configure-bindings)

## Channels

- [x] `openclaw channels add` — register Telegram/Slack/Discord bot accounts from `token_env`
- [ ] `openclaw channels remove` — tear down channel accounts (part of `claws clean`)
- [x] Default account selection (`channels.<kind>.defaultAccount`)
- [ ] Channel pairing / approval (`openclaw pairing approve`) — separate from apply, interactive
- [x] `ConfigMutationConflictError` retry logic

## Automations

- [ ] `automations[]` manifest block — probe/execute/clean scripts per machine
- [ ] Shell and `python@<ver>` script kinds
- [ ] `run_as` support (ssh user vs agent user)
- [ ] `manual: true` flag (skip during apply, run with `claws run`)
- [ ] File-based scripts (`probe_file`, `execute_file`, `clean_file`)

## Agent — missing features

- [ ] **Elevated tools** — global `tools.elevated.enabled` and `tools.elevated.allowFrom.<channel>` config
- [ ] **BOOTSTRAP.md** — write `agent.Bootstrap` content to workspace
- [ ] **Managed section markers** — `<!-- CLAWS MANAGED START/END -->` to preserve user content outside managed sections (current implementation does full file overwrite)

## Investigate — may not be needed

Items from v1 that manipulated JSON files directly instead of using `openclaw` CLI commands. Suspected problems from bypassing the CLI. Re-evaluate once we confirm whether the CLI covers these use cases natively.

- [ ] **Node exec-policy** — reconcile `exec_policy.security` and `exec_policy.ask` via `openclaw exec-policy set` (CLI exists, need to verify it works cleanly)
- [ ] **Exec-approvals on gateway** — per-agent security overrides in `~/.openclaw/exec-approvals.json` (v1 wrote raw JSON; no `openclaw config set` path exists for this file)
- [ ] **Auth-profiles** — ensure `<agentDir>/auth-profiles.json` exists (v1 copied from main agent or seeded empty JSON; unclear if the CLI handles this automatically now)
- [ ] **Memory notice** — write a dated memory notice on first workspace creation (nice-to-have, low priority)

## CLI commands

- [x] `claws apply`
- [x] `claws destroy` (plan exists)
- [ ] `claws clean` — remove orphaned machines/nodes/agents/channels not in manifest
- [ ] `claws status` — live dashboard of all resources
- [ ] `claws ssh` — SSH into a manifest machine
- [ ] `claws run` — execute manual automations
- [ ] `claws machines` / `gateways` / `nodes` / `channels` — list subcommands

## Intentionally dropped

- Kubernetes cluster support (`kubernetes-clusters`, `runtime_k8s.go`)
