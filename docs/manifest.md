# Manifest reference

A claws manifest is a single YAML file that describes **what a swarm looks
like** — the machines, the OpenClaw gateways on those machines, the nodes
paired to each gateway, the agents, their channels, and any custom
automations. `claws apply` converges your live infrastructure to match it.

This document is the field-by-field reference. For a hands-on introduction,
read [`quickstart.md`](quickstart.md) first.

The canonical Go definitions live in
[`internal/manifests/data/types.go`](../internal/manifests/data/types.go).
Validation rules are in
[`internal/manifests/data/validate.go`](../internal/manifests/data/validate.go).

---

## Top-level shape

```yaml
prefix:            string          # required; name-spaces every VM/host
env_file:          string          # optional; path relative to manifest dir
node_major:        int             # optional; Node.js major version to install
linode_token_env:  string          # optional; env var holding a Linode API token
allow_self:        bool            # optional; opt in to local (self) execution

machines:           [Machine]      # hosts to provision / connect to
gateways:           [Gateway]      # OpenClaw gateways (one per gateway host)
nodes:              [Node]         # exec nodes paired to a gateway
agents:             [Agent]        # agent profiles
automations:        [Automation]   # custom phases of bash/python/scp steps
```

### Top-level fields

| Field | Type | Required | Notes |
| ----- | ---- | -------- | ----- |
| `prefix` | string | yes | Prepended to every machine name (e.g. `prod` → `prod-gateway-host`). Required so parallel applies (or different users sharing a cloud account) never collide. |
| `env_file` | string | no | Path to a `.env` file with `KEY=VALUE` lines. Resolved relative to the manifest file. Keys referenced by `*_env` fields and automation `env:` allowlists are looked up here (process env wins on conflict). |
| `node_major` | int | no | Major Node.js version to install on every gateway/node. Defaults to whatever `internal/claws/plans/apply/common/install_nodejs.go` pins. |
| `linode_token_env` | string | needed for `type: linode` | Name of an env var that holds a Linode API token. The Linode provisioner reads it from the manifest env. |
| `allow_self` | bool | no | Opts this manifest into running on the operator's workstation (the "self" target) via local automation steps and `scp.upload` / `scp.download`. Without it, anything referencing `self` is rejected at load time. |

---

## `machines:`

Each entry describes one host. The provisioner uses `type` to pick a
driver; `sku` / `image` / `region` / `cpus` / `memory` / `disk` apply
per-driver. SSH identity is **not** embedded — claws uses the key
registered via `claws auth generate` / `claws auth use`.

```yaml
machines:
  - name: gateway-host          # local name (prefix is auto-prepended)
    type: multipass             # linode | ssh | multipass | self (reserved)
    sku:  g6-standard-2         # linode only
    image: linode/ubuntu24.04   # linode only
    region: us-east             # linode only
    cpus: 2                     # multipass only
    memory: 2G                  # multipass only
    disk: 10G                   # multipass only
    host: 1.2.3.4               # type: ssh only — pre-existing host's address
    internal_host: 10.0.0.7     # optional; used by other machines in the mesh
    ssh_port: 22                # optional
    bootstrap_user: root        # privileged user used ONLY to harden the box
    agent_user: agent           # unprivileged user every phase post-security uses
    ssh_key_env: SSH_KEY_PATH   # optional override for SSH key path
    arch: linux-arm64           # optional; defaults inferred from image/sku
    container: false            # internal; used by docker tests
    apply_security: false       # type: ssh only — opt-in to the security phase
```

| Field | Applies to | Notes |
| ----- | ---------- | ----- |
| `name` | all | Local-only identifier. Rejected if equal to `self` (reserved for local execution). |
| `type` | all | `linode`, `ssh`, `multipass`. `self` is synthetic and never appears here. `linode`/`multipass` are "hosted" types that get created + hardened by `provisioning` and `security` phases; `ssh` types skip provisioning and (by default) skip security too — pass `apply_security: true` to opt in. |
| `sku`, `image`, `region` | linode | Passed through to Linode's instance API. |
| `cpus`, `memory`, `disk` | multipass | Units match `multipass launch` (`1G`, `512M`, …). Zero/empty → provider defaults. |
| `host`, `ssh_port` | ssh | Required for pre-existing hosts. For hosted types they're filled in post-provisioning and persisted to local state. |
| `internal_host` | all | Address other manifest machines should use in-mesh. Defaults to `host`. |
| `bootstrap_user` | all | Privileged identity used ONLY during `provisioning` and `security` phases. Never used post-bootstrap. |
| `agent_user` | all | Unprivileged identity every post-security phase uses. |
| `arch` | all | `linux-x64`, `linux-arm64`, `darwin-x64`, `darwin-arm64`. Auto-detected for hosted types; required if you ship arch-specific binaries via automations. |
| `apply_security` | ssh | When `true`, run the security phase (`install-security-packages`, `enable-ufw`, `enable-fail2ban`, `enable-unattended-upgrades`) on this pre-existing host. Default `false`: claws leaves pre-provisioned SSH boxes alone. Requires the `bootstrap_user` to have passwordless sudo. Hosted types (`linode`/`multipass`) ignore this field — they always run security. |

### The reserved `self` target

You never declare `self` in `machines:`. It's an automatic target that
means "the machine running `claws`". An automation step with
`machines: [self]` executes locally (via `os/exec`), skipping SSH entirely.
Set `allow_self: true` at the top of the manifest to opt in.

Use it for `scp.upload` / `scp.download` and for build/prepare steps that
need your workstation's tools (e.g. a Go cross-compile before shipping a
binary).

---

## `gateways:`

An OpenClaw gateway runs on exactly one machine. You typically declare
one; a manifest can declare several if you're running multiple independent
swarms on the same hosts.

```yaml
gateways:
  - name: gateway                    # agents reference this name
    reference: gateway-host          # machine name to install on
    openclaw_version: "1.4.0"        # optional npm version pin
    networking:
      mode: headscale                # local | headscale | docker | linode_vpc
      public_hostname:
        strategy: sslip              # sslip | custom | fixed
        host: "http://foo.local:8080"  # strategy-dependent
      preauth_key_source: file       # file | env
      preauth_key_env: HEADSCALE_KEY # when source: env
      preauth_key_file: /var/lib/claws/headscale/preauth.key
    channels:
      - kind: telegram
        name: telegram-primary
        token_env: TELEGRAM_BOT_TOKEN
        default: true
```

### Gateway networking modes

| `mode` | What gets bound | When to use |
| ------ | --------------- | ----------- |
| `local` | openclaw-gateway listens on loopback only | Local dev, Docker integration tests. |
| `headscale` | Control plane on gateway; nodes dial it over Tailscale; openclaw listens on LAN. | Production / multi-host. |
| `docker` | Like headscale but scoped to a Docker network. | Integration tests. |
| `linode_vpc` | Binds to the Linode VPC interface. | Linode-only production. |

All modes except `local` imply a LAN (or mesh-LAN) bind; `local` implies
loopback. This is what the `configure-gateway` step probes against
`gateway.bind` in `openclaw.json`.

### `public_hostname` strategies

| Strategy | Meaning |
| -------- | ------- |
| `sslip` | Auto-derive a hostname from the gateway's public IP via sslip.io. Caddy + Let's Encrypt run automatically. |
| `custom` | `host:` is the full URL (e.g. `http://foo.local:8080`). Caddy/ACME is skipped — use for LAN/dev only. |
| `fixed` | `host:` is a hostname you own with DNS + certs already wired. |

### `preauth_key_source`

Headscale needs a preauth key so nodes can join without interactive login.

- `file` (recommended): `claws` uses the headscale CLI on the gateway to
  mint a key and stores it at `preauth_key_file` (default under
  `/var/lib/claws/headscale/`). First run creates it; subsequent runs
  reuse it.
- `env`: supply the key yourself via `preauth_key_env`. Useful when your
  headscale control plane is managed externally.

### `channels`

One entry per bot/account. Every channel is bound to the gateway; agents
pick up channels via `bindings:` below.

| Field | Notes |
| ----- | ----- |
| `kind` | `telegram`, `slack`, or `discord`. |
| `name` | Local identifier agents reference in `bindings[].account`. |
| `token_env` | Env var holding the bot token. Missing or empty keeps the binding but the bot won't reach its vendor API (the gateway stays healthy). |
| `target` | Optional channel-specific addressable identifier (e.g. a Slack workspace). |
| `default` | When `true`, agents without an explicit binding for this kind inherit this account. Max one default per kind. |

---

## `nodes:`

Exec nodes are remote shells agents can run tools on. One physical host
can back many logical nodes (rarely needed). Nodes are **paired** to a
gateway — a node only responds to exec requests coming via that gateway.

```yaml
nodes:
  - name: exec-node
    gateway: gateway            # must match a gateways[].name
    reference: node-host        # machine name the node runs on
    exec_policy:
      security: full            # read | write | full
      ask: "off"                # off | ask | always
```

### `exec_policy`

| Field | Values | Meaning |
| ----- | ------ | ------- |
| `security` | `read` | Read-only tools only. |
| `security` | `write` | Read + write in the workspace; no system mutations. |
| `security` | `full` | Arbitrary shell. Use only on isolated worker boxes. |
| `ask` | `off` | Agent executes without asking. |
| `ask` | `ask` | Agent asks the user (via the bound channel) before executing. |
| `ask` | `always` | Every tool call is confirmed. |

Tighten these on any shared/production box. `full` + `off` is only safe
when the node is disposable (e.g. a dedicated worker VM).

---

## `agents:`

Each agent is a profile the gateway serves. Agents are created, their
workspaces populated (`SOUL.md`, `IDENTITY.md`, `AGENTS.md`), their model
pinned, exec policy wired, and channel bindings installed — all by the
`agents` phase.

```yaml
agents:
  - id: dev
    gateway: gateway
    workspace: ~/.openclaw/workspace-dev
    model:
      primary:   "anthropic/claude-opus-4-6"
      fallbacks: ["anthropic/claude-sonnet-4-6"]
    tools:
      exec:
        host: node              # gateway | node
        node: exec-node         # when host: node
        security: full
      elevated:
        enabled: true
        allow_from:
          telegram: ["123456"]  # optional user-ID allowlist per channel kind
    soul: |                     # multi-line personality / role prose
      ...
    agents_md: |                # multi-line runtime instructions (AGENTS.md)
      ...
    identity:
      name: Dev
      emoji: "💻"
    bindings:
      - channel: telegram
        account: telegram-main  # omit to use the default telegram account
```

### Agent fields

| Field | Notes |
| ----- | ----- |
| `id` | Unique within the manifest. Becomes the agent's identifier in openclaw CLI/REST. Trimmed and lowercased by the reader. |
| `gateway` | Must match a `gateways[].name`. |
| `workspace` | Absolute path (or `~/…`) on the gateway host. The workspace is where `SOUL.md`, `IDENTITY.md`, `AGENTS.md`, and agent state live. Two agents MUST NOT share a workspace. |
| `model.primary` | Fully-qualified model ref (`provider/id`). Custom providers (anything outside the built-in catalog) need a matching entry in `openclaw.json` — usually installed by an automation. |
| `model.fallbacks` | Tried in order when primary fails. |
| `tools.exec.host` | `gateway` (run on the gateway box) or `node` (run on a paired node). |
| `tools.exec.node` | Required when `host: node`. Must match a `nodes[].name` paired to the same gateway. |
| `tools.exec.security` | Overrides the node's `exec_policy.security` for this agent. |
| `tools.elevated.enabled` | Turn elevated tools (sudo, system mutations) on/off. Default: off. |
| `tools.elevated.allow_from` | Map `channel-kind → [user-ids]`. Only these users can authorize elevated calls. Empty = anyone bound to the channel. |
| `soul` | Free-form personality prose. Written verbatim to `SOUL.md`. |
| `agents_md` | Runtime instructions (tools available, etiquette, hand-off rules). Written to `AGENTS.md`. |
| `identity.name`, `identity.emoji` | Display name / emoji rendered by channel integrations. |
| `bindings[]` | Wires the agent to channels. `channel` matches `channels[].kind`; `account` matches `channels[].name`. Omit `account` to inherit the kind's default. |

---

## `automations:`

Custom phases of steps that run alongside the built-in ones. Each
automation becomes one scaffold phase.

```yaml
automations:
  - name: install-toolchain        # becomes the phase name
    machines: [gateway-host]       # one or more machines (or "self")
    manual: false                  # true → skipped by `claws apply`
    concurrency: 1                 # max parallel targets (default: provider cap)
    run_as: agent                  # override default SSH user for this phase
    env:                           # allowlist of env vars exported to steps
      - GITHUB_TOKEN
    steps:
      - name: install-tool
        kind: bash                 # bash | python | scp.upload | scp.download
        run_as: agent              # per-step override
        env: [HTTP_PROXY]          # per-step allowlist (unions with phase env)
        applicable: "true"         # optional; step runs only if this exits 0
        check: "command -v mytool" # skip execute when check succeeds
        execute: |
          ... mutation ...
        verify: "mytool --version" # confirm after execute
```

Every `*` script has a `*_file:` twin (`check_file`, `execute_file`, …)
that reads the script from a file relative to the manifest directory —
handy for anything non-trivial.

### Step kinds

#### `bash` (default) and `python`

The usual shape. All four lifecycle hooks are optional; an empty
`execute` with a matching `check` makes the step a pure probe (useful for
gating later automations).

- `applicable` — skips the step entirely when it exits non-zero.
- `check` — when it exits 0, the step is already satisfied; `execute`
  doesn't run.
- `execute` — mutation body.
- `verify` — runs after `execute`; non-zero fails the step.

Scripts run with the target's default shell. Env comes from the union
of phase `env:` and step `env:` allowlists; values resolve from process
env first, then `env_file`.

#### `scp.upload` and `scp.download`

File/directory transfers between the operator (`self`) and a remote
machine. `machines:` must list at least one remote; `self` is the implicit
other side. Require `allow_self: true`.

| Field | Meaning |
| ----- | ------- |
| `source` | For upload: path on the operator. For download: path on the remote. Relative paths resolve against the manifest directory (upload) or the agent home (download). |
| `destination` | Mirror of `source`. |
| `mode` | Optional octal (`"0755"` or `"755"`) applied post-transfer. |
| `if_changed` | `scp.upload` only. Flips the default `check` to "SHA-256 match between local source and remote destination" — the step becomes content-addressed, no hand-rolled check needed. |

Directories are auto-detected and copied recursively; symlinks are
followed.

### What automations are for

Automations are the **extension point** for everything the built-in
phases don't cover. They let you bootstrap machines with extra binaries,
config, services, or any scripted setup a specific deployment needs,
without ever touching the claws source code. Typical uses:

- **Install compilers and toolchains** on nodes so agents can build
  projects there (Go, Rust, Node versions beyond what claws installs for
  openclaw itself, Foundry, protoc, …).
- **Ship a local binary** you just built on your workstation to a
  gateway or node (`kind: scp.upload`), then wire it up as a systemd
  user unit on the remote side.
- **Install and configure a model provider** that openclaw doesn't ship
  a built-in driver for — e.g. a self-hosted inference server, a local
  proxy, or a custom OpenAI-compatible endpoint. The automation patches
  `~/.openclaw/openclaw.json` so agents can reference
  `my-provider/some-model` in `model.primary` like any other model.
- **Seed data** on nodes: clone repos into workspaces, pre-warm caches,
  install language servers, drop SSH keys for git access.
- **Wire auxiliary services** (Caddy overrides, cron jobs, log shippers,
  backup scripts, health probes) that live alongside the openclaw
  gateway but aren't part of it.

Anything you can script with bash, python, or an SFTP transfer is fair
game. Because every step has its own `check` / `execute` / `verify`,
your automation is idempotent from day one — re-running `claws apply`
skips work that's already done.

### Targeting `self`

- `self` steps execute locally with cwd pinned to the manifest
  directory, so relative paths in your scripts resolve predictably.
  `$CLAWS_MANIFEST_DIR` also holds the absolute path.
- `machines: [self]` automations and any `scp.*` step require
  `allow_self: true` at the top of the manifest.
- An `scp.upload` / `scp.download` automation MUST NOT list `self` in
  `machines:` — `self` is implicit.

### `manual: true`

Manual automations are included in the plan but **skipped by
`claws apply`**. Run them explicitly:

```bash
claws automations apply -f manifest.yml --name <automation-name>
# or include all of them in the regular apply
claws apply -f manifest.yml --include-manual-automations
```

Use this for steps that depend on side effects `apply` can't guarantee —
e.g. a step that patches `openclaw.json`, which only exists after the
gateway phase has already run.

### Ordering

Automations execute in manifest order, **after** the built-in phases that
they don't depend on and **before** ones that would. The built-in
ordering is `provisioning → security → mesh → gateway → channels →
node → agents`. Custom automations slot in so that:

- `machines: [self]` automations run early (they don't need any remote
  phase to finish).
- Remote automations run after `security` (so the agent user exists).
- Manual automations are emitted but withheld by default.

Within an automation, steps are executed in listed order; inside a step,
targets fan out concurrently up to `concurrency`.

---

## Validation

`claws` runs a narrow static check on load (see
[`internal/manifests/data/validate.go`](../internal/manifests/data/validate.go))
that refuses the manifest if it would do something genuinely unsafe:

- A machine literally named `self` (shadows the reserved target).
- An automation referencing `self` with `allow_self: false`.
- An `scp.upload` / `scp.download` step missing `source` or
  `destination`, or carrying a non-octal `mode`.
- A `scp.*` automation with no remote machine (both sides would be
  `self`).
- An `scp.*` automation that listed `self` explicitly (it's implicit).
- A step with an unknown `kind`.
- `if_changed: true` on any kind other than `scp.upload`.

Everything else (duplicate names, missing gateway references, typos in
model ids) is caught later by the phase builders or openclaw itself, with
clearer phase-specific errors.

---

## A worked `automations:` example

A single automation that installs Go on one or more remote hosts. It
shows the full `bash` step lifecycle — `check` short-circuits when Go is
already there, `execute` performs the mutation, `verify` confirms it took
effect.

```yaml
automations:
  - name: install-golang
    machines: [gateway-host, node-host]
    concurrency: 2           # up to 2 targets at a time
    run_as: agent            # unprivileged user; `execute` uses sudo where needed
    steps:
      - name: install-go
        kind: bash
        # Re-runs are free: if /usr/local/go/bin/go already exists and is
        # at least the version we want, we skip execute entirely.
        check: |
          command -v /usr/local/go/bin/go >/dev/null && \
            /usr/local/go/bin/go version | grep -qE 'go1\.(2[5-9]|[3-9][0-9])'
        execute: |
          set -euo pipefail
          GO_VERSION="1.25.0"
          case "$(uname -m)" in
            x86_64|amd64)  ARCH=amd64 ;;
            aarch64|arm64) ARCH=arm64 ;;
            *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
          esac
          curl -fsSL -o /tmp/go.tgz \
            "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz"
          sudo rm -rf /usr/local/go
          sudo tar -C /usr/local -xzf /tmp/go.tgz
          rm -f /tmp/go.tgz
          # Put go on PATH for future interactive shells.
          if ! grep -q '/usr/local/go/bin' "$HOME/.bashrc" 2>/dev/null; then
            echo 'export PATH="/usr/local/go/bin:$PATH"' >> "$HOME/.bashrc"
          fi
        verify: /usr/local/go/bin/go version
```

Things to notice:

- **`check` gates `execute`.** On a converged host the check succeeds and
  the step is reported as satisfied in milliseconds — re-running
  `claws apply` is cheap.
- **Check the specific state you want**, not just "is the binary there":
  the regex here also guards the version floor, so bumping `GO_VERSION`
  triggers a reinstall automatically.
- **Targets fan out.** `concurrency: 2` lets both hosts install Go in
  parallel; drop it to `1` for a serial roll-out.
- **`run_as: agent`** means the SSH session runs as the unprivileged
  agent user; `execute` calls `sudo` explicitly for the bits that need
  root. This is the recommended pattern — never open a root SSH session
  just to write one file.

For more complex shapes (local builds with `machines: [self]`, SFTP
transfers via `scp.upload` / `scp.download`, content-addressed uploads
with `if_changed`), see the step-kind reference above.

Use `claws manifest validate -f <path>` to sanity-check a manifest
without running anything.
