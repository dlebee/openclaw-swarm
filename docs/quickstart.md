# Quick start

A guided walk-through of getting a **two-VM swarm** (one gateway, one node,
two agents) running on your laptop with Multipass in under ten minutes.
The same flow, with different `type:` on each machine, brings up a
Linode-backed production deployment.

You'll need your own manifest file for the steps below. See
[`manifest.md`](manifest.md) for the field-by-field reference, and run
`claws manifest validate -f path/to/manifest.yml` before your first apply
to catch YAML and structural errors early. Throughout this guide we'll
refer to your manifest as `path/to/manifest.yml`.

---

## 0. Prerequisites

- **Go 1.25+** (only to build `claws` itself).
- A provisioner that matches your manifest's `machines[].type`:
  - `multipass` — `brew install multipass` (macOS) or
    `sudo snap install multipass` (Linux).
  - `linode` — a Linode API token exported via the env var named in
    `linode_token_env:`.
  - `ssh` — pre-existing hosts reachable over SSH; nothing to install.
- An SSH key pair. Don't have one? `claws ssh auth` will create one for
  you.
- Bot tokens for any `channels:` you declared (Telegram/Slack/Discord).
  Leaving the relevant env var empty keeps the binding but the bot won't
  reach its vendor API.

## 1. Build the CLI

```bash
cd openclaw-swarm
go build -o claws ./cmd/cli
./claws --help
```

## 2. Register your SSH identity with claws

`claws` uses one SSH key for every dial it makes (provisioning, apt,
openclaw CLI invocations, SFTP, …). Tell it which key to use:

```bash
./claws ssh auth
```

It will either pick up `~/.ssh/id_ed25519` / `~/.ssh/id_rsa` or offer to
generate a fresh key. Either way the public half ends up on every machine
`claws` provisions.

## 3. Fill in the env file

If your manifest sets `env_file:`, claws resolves that path **relative to
the manifest file**. Create the env file next to your manifest and put
the env vars your manifest references there:

```bash
cat > path/to/.env <<'EOF'
# One line per env var referenced by the manifest.
# `*_env:` fields (token_env, linode_token_env, preauth_key_env, …)
# and automations' `env:` allowlists look values up here.
MY_BOT_TOKEN=...
LINODE_TOKEN=...
EOF
```

Values defined in your process env win over the file — useful for
overriding a single token in CI.

## 4. Preview the plan

```bash
./claws apply -f path/to/manifest.yml --dry-run
```

You'll see a colored table of phases → targets → steps. Each cell shows
whether the step is **satisfied** (green, skip), **pending** (needs to
run), or **not applicable** (grey, skipped on this target). The preview
never mutates anything — reads are cached against a live snapshot of
`openclaw.json` so repeat previews are cheap.

> Tip: `--pretty-plan` renders the same preview inside a Bubble Tea
> viewport you can scroll. `--list-phases` prints just the phase names in
> order.

## 5. Apply

```bash
./claws apply -f path/to/manifest.yml
```

You'll get one confirmation prompt, then watch the plan execute. Phases
run in order; inside a phase, targets fan out concurrently (capped per
phase — gateway phases run 5-wide, provisioning 10-wide, etc.). Expect
the first apply on a fresh cluster to be dominated by VM creation
(`multipass launch` / Linode create) and apt installs — **~5–8 minutes**
on a modern laptop for a small Multipass deployment; longer on Linode.

A successful run ends with every cell green.

### Scoping a run

```bash
# Only specific phases
./claws apply -f path/to/manifest.yml --phases mesh,gateway

# Everything EXCEPT provisioning (useful after a first apply)
./claws apply -f path/to/manifest.yml --skip-phases provisioning

# Assume yes (CI / scripting)
./claws apply -f path/to/manifest.yml --yes

# List the phase names for this manifest in order
./claws apply -f path/to/manifest.yml --list-phases
```

## 6. Run any `manual: true` automations

Automations flagged `manual: true` are included in the plan but skipped by
`claws apply`. Run them explicitly whenever a step depends on a side
effect `apply` can't guarantee (e.g. the gateway phase having already
written `~/.openclaw/openclaw.json` that the automation then patches):

```bash
./claws automations apply -f path/to/manifest.yml --name <automation-name>
# or bundle them into the regular apply:
./claws apply -f path/to/manifest.yml --include-manual-automations
```

## 7. Verify the swarm is alive

```bash
# Openclaw gateway status from your machine
./claws gateways status -f path/to/manifest.yml

# Open a shell on any manifest-declared machine using the same identity
# claws uses for apply
./claws remote ssh <machine-name>

# On the gateway itself, tail the daemon and talk to an agent
systemctl --user status openclaw-gateway
journalctl --user -u openclaw-gateway -f
openclaw chat <agent-id>
```

## 8. Iterate

Edit the manifest, re-apply. Every step's `check` short-circuits
`execute` when the work is already done, so re-runs are fast and
non-destructive:

```bash
./claws apply -f path/to/manifest.yml
```

Typical loops:

- **Change a model** → re-apply; only `ensure-model` runs for affected
  agents.
- **Add a channel** → re-apply; `add-channels` picks it up, existing
  bindings stay untouched.
- **Add a new agent** → re-apply; `add-agent` creates the profile, the
  workspace step writes its `SOUL.md`/`AGENTS.md`, bindings wire it to
  channels.
- **Bump the openclaw version** → set `openclaw_version:` on the
  gateway, re-apply; `install-openclaw` runs npm again on every host.

## 9. Tear down

```bash
./claws destroy -f path/to/manifest.yml
# and optionally:
./claws clean
```

`destroy` removes the provisioned machines (Multipass delete / Linode
delete). `clean` removes local claws state (plan cache, SSH known-hosts).
`type: ssh` machines are never deleted — `claws` has no authority over
pre-existing hosts.

## Where to go next

- **[`manifest.md`](manifest.md)** — every field, what it does, and the
  validation rules.
- **[`scaffold.md`](scaffold.md)** — the runner internals, if you want to
  write your own automation steps or a new phase.
- **[`running-integration-tests.md`](running-integration-tests.md)** —
  how to exercise the three test tiers.

## Common gotchas

- **`manifest references reserved target "self"`** — set
  `allow_self: true` at the top of the manifest. Without it, any
  automation targeting `machines: [self]` or using `scp.upload` /
  `scp.download` is rejected at load time. This is a guardrail, not a
  bug: a manifest pulled from a repo should never silently run bash on
  your workstation.
- **Preview is fast but apply is slow** — normal. Probe reads are
  snapshotted; mutations aren't, because each one needs the fresh
  server state.
- **`claws apply` skipped my automation** — automations with
  `manual: true` are skipped by default. Pass
  `--include-manual-automations` or run them explicitly with
  `claws automations apply --name …`.
- **A `multipass launch` hung** — Multipass occasionally wedges its
  image cache. `multipass purge` and re-apply usually fixes it. The
  provisioning phase is idempotent, so re-applying is safe.
