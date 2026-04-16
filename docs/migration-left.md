# Migration: what's left from openclaw-swarm v1

Tracking parity between `openclaw-swarm` (v1) and `openclaw-swarm2`.
Items are grouped by area; checked items are implemented in swarm2.

## Apply phases

- [x] Provisioning (create Linode machines, authorize SSH keys, ensure agent user)
- [x] Security (ufw, fail2ban, unattended-upgrades, security packages)
- [ ] Mesh — headscale install, tailscale join, Caddy HTTPS reverse proxy, preauth key management
- [x] Gateway (install nodejs/openclaw, bootstrap, configure, pair device)
- [x] Node (install nodejs/openclaw, bootstrap, configure, exec-policy, pair)
- [x] Agents (add, ensure-model, configure-workspace, configure-tools, configure-bindings)

## Channels

- [x] `openclaw channels add` — register Telegram/Slack/Discord bot accounts from `token_env`
- [x] `openclaw channels remove` — tear down channel accounts (`claws clean channels`)
- [x] Default account selection (`channels.<kind>.defaultAccount`)
- [x] Channel pairing / approval (`claws channels pair` — interactive, `openclaw pairing approve`)
- [x] `ConfigMutationConflictError` retry logic

## Automations

- [ ] `automations[]` manifest block — probe/execute/clean scripts per machine
- [ ] Shell and `python@<ver>` script kinds
- [ ] `run_as` support (ssh user vs agent user)
- [ ] `manual: true` flag (skip during apply, run with `claws run`)
- [ ] File-based scripts (`probe_file`, `execute_file`, `clean_file`)

## Agent — missing features

- [x] **Elevated tools** — global `tools.elevated.enabled` and `tools.elevated.allowFrom.<channel>` config
- [x] **Managed section markers** — `<!-- CLAWS MANAGED START/END -->` to preserve user content outside managed sections

## Investigate — may not be needed

Items from v1 that manipulated JSON files directly instead of using `openclaw` CLI commands. Suspected problems from bypassing the CLI. Re-evaluate once we confirm whether the CLI covers these use cases natively.

- [x] **Node exec-policy** — `openclaw exec-policy set --security <val> --ask <val>` (clean CLI, no JSON manipulation)
- [x] **Exec-approvals on gateway** — NOT NEEDED. `agents.list[N].tools.exec.security` via `openclaw config set` is sufficient. Integration test confirms full exec pipeline works without `exec-approvals.json`. See `docs/missconceptions/exec-policy.md`.
- [ ] **Auth-profiles** — ensure `<agentDir>/auth-profiles.json` exists (v1 copied from main agent or seeded empty JSON; unclear if the CLI handles this automatically now)
- [ ] **Memory notice** — write a dated memory notice on first workspace creation (nice-to-have, low priority)

## CLI commands

- [x] `claws apply`
- [x] `claws destroy` (plan exists)
- [x] `claws clean channels` — remove orphaned channel accounts (extensible to machines/nodes/agents)
- [ ] `claws status` — live dashboard of all resources
- [x] `claws ssh` — SSH into a manifest machine (interactive picker or `--name`)
- [ ] `claws run` — execute manual automations
- [x] `claws channels pair` — interactive channel pairing
- [ ] `claws machines` / `gateways` / `nodes` — list subcommands

## Intentionally dropped

- Kubernetes cluster support (`kubernetes-clusters`, `runtime_k8s.go`)
