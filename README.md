# Claws — OpenClaw swarm orchestrator

`claws` is the CLI that turns a single YAML manifest into a working swarm of
OpenClaw agents running across one or more machines. It drives the full
lifecycle — provision, harden, mesh, install, configure, pair, and run — and
is **idempotent by design**: every step probes before it acts, so re-running
`claws apply` on a converged cluster is cheap and safe.

## What it does for you

Given a manifest, `claws apply` will:

1. **Provision** cloud or local VMs (Linode, Multipass) — or just accept a
   list of pre-existing SSH hosts.
2. **Harden** them (UFW, fail2ban, unattended-upgrades, a non-root agent
   user).
3. **Mesh** them over Headscale/Tailscale when you need a private overlay.
4. **Install** Node.js + the OpenClaw CLI on every gateway and node.
5. **Bootstrap** the OpenClaw gateway, **pair** its local device, and
   **pair** every node into that gateway.
6. **Register** channels (Telegram/Slack/Discord accounts) on the gateway.
7. **Configure** every agent — model, workspace (`SOUL.md`, `IDENTITY.md`,
   `AGENTS.md`), exec policy, elevated-tool allowlists, and channel
   bindings.
8. Run your **automations** (custom bash / python / scp steps with their
   own check/execute/verify lifecycle) inline with the built-in phases.

See [`docs/quickstart.md`](docs/quickstart.md) to get a two-VM swarm
running locally in a few minutes, and [`docs/manifest.md`](docs/manifest.md)
for the full field-by-field reference.

## Install & build

```bash
git clone <this-repo>
cd openclaw-swarm
go build -o claws ./cmd/cli
# or: go run ./cmd/cli -f path/to/manifest.yml apply
```

Go 1.25+ required. No runtime deps for the binary itself — everything else
(multipass, headscale, node, openclaw, caddy, …) is installed **on the
target machines** by the apply plan.

## One-minute demo

```bash
# 1. Register your SSH key with claws (one-time)
./claws ssh auth

# 2. See the plan without touching anything
./claws apply -f path/to/your/manifest.yml --dry-run

# 3. Apply
./claws apply -f path/to/your/manifest.yml

# 4. Re-apply — every cell should report "satisfied" and exit in seconds
./claws apply -f path/to/your/manifest.yml
```

## Top-level commands

| Command | What it does |
| ------- | ------------ |
| `claws apply -f manifest.yml` | Build the plan, show a preview, then provision + configure everything. `--dry-run` skips mutations; `--phases` / `--skip-phases` scope the run; `--list-phases` prints the phase list for this manifest. |
| `claws automations apply -f manifest.yml [--name X]` | Run automations explicitly — including ones flagged `manual: true`, which `apply` skips. |
| `claws destroy -f manifest.yml` | Tear down every machine the manifest provisioned. |
| `claws clean` | Remove local state (cached plan, SSH known-hosts) for a fresh run. |
| `claws channels …` | Interact with a live gateway's channels config. |
| `claws gateways …` | Interact with a live gateway (status, pair devices, restart). |
| `claws remote ssh <machine>` | Open an SSH session to a manifest-declared machine using the same identity `apply` uses. |
| `claws ssh auth` | Import / generate the SSH key pair `claws` uses for every dial. |
| `claws manifest …` | Validate, lint, and inspect a manifest without running anything. |
| `claws github …` | GitHub integration helpers. |

`claws -f <manifest>` is a persistent flag — most subcommands pick up the
manifest from there.

## Architecture in one paragraph

A manifest is parsed into a `scaffold.Plan` of **phases** (provisioning,
security, mesh, gateway, node, channels, agents, plus your custom
automations). Each phase fans out over its **targets** (machines, agents,
nodes…) and runs a fixed list of **steps**. Every step has four methods —
`Applicable`, `Check`, `Execute`, `Verify` — so the runner can probe
concurrently before it mutates and confirm after. Config reads against the
gateway's `openclaw.json` are cached in-process during the probe phase via a
snapshot reader so preview latency stays sub-second even on cold 1-vCPU
hosts.

See [`docs/scaffold.md`](docs/scaffold.md) for the runner internals and
[`internal/claws/plans/apply/AGENTS.md`](internal/claws/plans/apply/AGENTS.md)
for the phase-by-phase walkthrough.

## Testing

Three integration tiers, each behind a build tag — see
[`docs/running-integration-tests.md`](docs/running-integration-tests.md):

```bash
go test ./...                                                        # unit tests
go test -tags=integration_docker    ./test/integration/docker/...    # containers
go test -tags=integration_multipass ./test/integration/multipass/... # local VMs
go test -tags=integration_linode    ./test/integration/linode/...    # real cloud
```

## Documentation map

- **[docs/quickstart.md](docs/quickstart.md)** — get a local two-VM swarm
  running end-to-end.
- **[docs/manifest.md](docs/manifest.md)** — complete reference for every
  manifest field.
- **[docs/scaffold.md](docs/scaffold.md)** — how phases, steps, and the
  runner fit together.
- **[docs/running-integration-tests.md](docs/running-integration-tests.md)** —
  running the three integration-test tiers.
- **[docs/multipass-integration-plan.md](docs/multipass-integration-plan.md)** —
  design notes for the Multipass tier.
- **[internal/claws/plans/apply/AGENTS.md](internal/claws/plans/apply/AGENTS.md)** —
  contributor walkthrough of each phase's steps.
- **[docs/issues/](docs/issues/)** — historical design traps and how they
  were resolved.
