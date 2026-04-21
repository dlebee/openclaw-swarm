# Multipass Integration Test Mode — Plan

## Why this exists

Our container-based integration tests in `test/integration/` run against a
stubbed `systemctl` (`test/infra/stub-systemctl.sh` installed over
`/usr/local/bin/systemctl` in `test/infra/Dockerfile`). The stub accepts
every verb/unit combination and returns 0. That masks an entire class of
bug where code writes the correct unit file but activates the wrong one —
see `docs/issues/04-node-install-enables-gateway-unit.md`, where a node
install silently "enabled" `openclaw-gateway.service` in CI and only failed
on real Linode VMs.

Two problems today:

1. **Low fidelity.** Docker containers aren't systemd. User-bus units,
   linger, cloud-init ordering, and real unit activation aren't exercised.
2. **Slow feedback on the real thing.** Linode apply/destroy cycles cost
   2–5 minutes each, plus API quotas and a billed account.

This plan introduces a **third integration tier**: a local Multipass-based
test mode that runs the same manifests against real Ubuntu VMs with real
systemd, over the LAN, with no stubs. Close enough to Linode for the code
paths we actually care about; fast enough to run on a laptop.

## Scope

**In scope**

- A `multipass` machine provider that implements `hosting.Provider`.
- A parallel manifest variant (`manifests/army-local.yml`) that uses the
  existing plain-HTTP control URL path to skip Caddy + TLS.
- Optional small enhancement to `mesh/control_url.go` to avoid hard-coding
  the gateway LAN IP in the manifest.
- Integration test harness that can drive this tier with the same
  `claws apply` flow used in CI and production.

**Out of scope**

- Public TLS / Let's Encrypt testing. Plain HTTP is intentional.
- Multi-host / multi-machine testing across physical boxes. All VMs live on
  one developer host or one CI runner.
- Replacing the Docker tier. The Docker tier stays for fast pure-logic
  tests (plan graph, cache, CLI). Multipass is additive.

## Why Multipass specifically

| Property                         | Multipass | Docker + stub | libvirt/KVM | Linode |
| -------------------------------- | --------- | ------------- | ----------- | ------ |
| Real systemd + user bus          | yes       | no            | yes         | yes    |
| Real cloud-init                  | yes       | no            | yes         | yes    |
| Works on macOS and Linux dev     | yes       | yes           | Linux only  | yes    |
| Boot time                        | ~10–30s   | <1s           | ~10s        | ~60s   |
| Matches Ubuntu cloud image       | yes       | no            | yes         | yes    |
| Catches issue-04-class bugs      | yes       | no            | yes         | yes    |

We pick Multipass over raw libvirt because it runs identically on a Mac
laptop (via Hypervisor.framework) and a Linux CI runner (via KVM), with
one CLI. Our manifests target `linode/ubuntu24.04` today, so the
Ubuntu-only limitation isn't a real constraint.

## Architecture fit

### 1. The provider seam already exists

`internal/hosting/hosting.go` defines:

```29:37:internal/hosting/hosting.go
type Provider interface {
	Kind() string
	CreateInstance(ctx context.Context, opts CreateInstanceOpts) (*Instance, error)
	DeleteInstance(ctx context.Context, resourceID string) error
	WaitRunning(ctx context.Context, resourceID string) (*Instance, error)
	ListByTag(ctx context.Context, tag string) ([]Instance, error)
}
```

A `multipass` provider is a peer of the existing Linode provider. The
manifest-level type is already open-ended:

```3:8:internal/manifests/data/types.go
type MachineType string

const (
	MachineTypeLinode MachineType = "linode"
	MachineTypeSSH    MachineType = "ssh"
```

We add `MachineTypeMultipass MachineType = "multipass"` and register a
`hosting.Provider` implementation against it.

### 2. Caddy is already skippable when the control URL is plain HTTP

```25:39:internal/claws/plans/apply/mesh/install_caddy.go
func (*InstallCaddyStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MeshTarget)
	if !ok || !mt.IsGatewayHost {
		return false, nil
	}
	if v, ok := scaffold.PlanCacheGet(ctx, CacheKeyControlURL); ok {
		controlURL, _ := v.(string)
		return !IsHTTPControlURL(controlURL), nil
	}
	return !ExpectedControlURLIsHTTP(mt.Gateway), nil
}
```

And `resolveControlURL` already honors a `custom` strategy with an explicit
`http://` host:

```81:89:internal/claws/plans/apply/mesh/control_url.go
case "custom":
    if gw.Networking.PublicHostname == nil || strings.TrimSpace(gw.Networking.PublicHostname.Host) == "" {
        return "", fmt.Errorf("networking.public_hostname.host is required when strategy is custom")
    }
    h := strings.TrimSpace(gw.Networking.PublicHostname.Host)
    if strings.HasPrefix(h, "https://") || strings.HasPrefix(h, "http://") {
        return h, nil
    }
    return "https://" + h, nil
```

This means **LAN mode is a manifest shape, not a code fork**. The entire
apply plan below the networking layer runs identically.

### 3. Provisioning-phase resolver must be generalized

`internal/claws/plans/apply/provisioning/resolve.go` currently hard-codes
Linode:

```20:45:internal/claws/plans/apply/provisioning/resolve.go
func ResolveLinodeInstances(ctx context.Context, provider hosting.Provider, prefix string, targets []scaffold.Target) error {
    ...
    if mt.Spec.Type != manifestdata.MachineTypeLinode {
        continue
    }
    ...
    instances, err = provider.ListByTag(ctx, prefixTag)
```

We'll either rename/generalize this to `ResolveHostedInstances` over any
non-SSH provider type, or add a sibling resolver for Multipass. Either way
the contract is: for each non-SSH machine, seed `mt.Instance.PublicIPv4`
(which for Multipass is the bridge LAN IP) and record it with
`scaffold.RecordPlanMachineHost`.

## Deliverables

### D1. `internal/hosting/multipass/` — provider implementation

A single package shelling out to the `multipass` CLI. Methods map cleanly:

| `hosting.Provider`       | `multipass` call                                      |
| ------------------------ | ----------------------------------------------------- |
| `Kind()`                 | returns `"multipass"`                                 |
| `CreateInstance`         | `multipass launch --name <label> --cpus N --memory X --disk Y --cloud-init <seed>` |
| `WaitRunning`            | `multipass info <label> --format json` (poll)         |
| `DeleteInstance`         | `multipass delete <label> --purge`                    |
| `ListByTag`              | `multipass list --format json` + sidecar tag file     |

Notes:

- **Tags.** Multipass has no tag primitive. Store a tag set per instance in
  a sidecar file under `~/.cache/openclaw/multipass/tags/<label>.json`.
  `ListByTag` enumerates instances, reads sidecars, filters. Cheap, local,
  no network.
- **IP.** `multipass info --format json` returns `ipv4`. First entry is the
  bridge IP. That becomes `Instance.PublicIPv4`.
- **Cloud-init.** Generate a NoCloud seed from `CreateInstanceOpts`
  (`PublicKeys`, `Label`, `RootPass`) that mirrors the Linode equivalent —
  specifically, SSH-authorized keys for the agent user and `sudo` without
  password. Store the rendered seed per-instance under
  `~/.cache/openclaw/multipass/seeds/<label>.yaml` for postmortem.
- **Uniqueness.** Label = `<prefix>-<machine.name>`, matching the Linode
  convention in `machineLabel()`.

### D2. Machine schema additions

Extend `internal/manifests/data/types.go` `Machine` with optional fields
that only apply when `type: multipass`:

```go
CPUs   int    `yaml:"cpus,omitempty"`
Memory string `yaml:"memory,omitempty"` // e.g. "2G"
Disk   string `yaml:"disk,omitempty"`   // e.g. "10G"
```

And `MachineTypeMultipass MachineType = "multipass"`.

Validate in `internal/manifests/service/` that these are only set when
`type: multipass` (or simply ignored otherwise — keep lenient).

### D3. Generalized provider resolver

Rename/refactor `ResolveLinodeInstances` to work over any non-SSH
`hosting.Provider`, driven by `machine.Type`. Call sites in
`internal/cli/commands/automations/automations.go` and
`internal/claws/plans/destroy/destroy.go` (per the existing grep) need to
pass the right provider.

### D4. Parallel manifest — `manifests/army-local.yml`

Mirrors `manifests/army.yml` with three classes of change:

- `type: linode` → `type: multipass`; drop `sku`/`region`; add
  `cpus/memory/disk`.
- `linode_token_env:` removed.
- `public_hostname`: switch from `{ strategy: sslip }` to explicit HTTP:

```yaml
public_hostname:
  strategy: custom
  host: "http://army-local-gateway-host.local:8080"
```

Everything below (channels, agents, nodes, automations) is byte-identical.
See "Gateway host value options" below for alternatives to the mDNS
hostname.

### D5. (Optional) Sentinel control URL

To avoid checking a hostname or IP into the manifest, teach
`resolveControlURL` in `mesh/control_url.go` to accept `http://:PORT` (or a
sentinel like `http://@lan:PORT`) and substitute the gateway machine's
resolved `PublicIPv4`. Seven-line change, fully backward compatible.

If we ship D5, the manifest becomes truly static:

```yaml
public_hostname:
  strategy: custom
  host: "http://:8080"
```

### D6. Integration test harness

- New build tag: `//go:build integration_multipass` (parallel to the
  existing `//go:build integration`).
- Test lifecycle:
  1. Preflight: check `multipass` is on PATH; `multipass version`; fail
     fast with a clear "install Multipass" message if missing.
  2. Launch VMs from `manifests/army-local.yml` via `claws apply`.
  3. Assert on real systemd: `systemctl --user is-enabled openclaw-node`,
     `systemctl is-enabled openclaw-gateway`, headscale reachable at
     `http://<gateway-ip>:8080/health`, tailscale `Status` reports online.
  4. Teardown: `claws destroy -f manifests/army-local.yml --all --yes`,
     then `multipass delete --all --purge` as a belt-and-braces cleanup.
- **No stubs.** Do not install `stub-systemctl.sh` on these VMs. The whole
  point is real systemd.

### D7. CI integration

GitHub Actions on `ubuntu-latest` supports KVM now. Add a new job:

```yaml
integration-multipass:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - name: Install Multipass
      run: sudo snap install multipass
    - name: Run Multipass integration tests
      run: go test -tags=integration_multipass ./test/integration/...
      timeout-minutes: 20
```

Runs in parallel with the existing Docker-based `integration` job. Failure
in either fails the PR.

## Gateway host value options

The `public_hostname.host` in `army-local.yml` has three possible shapes.
Pick one; they're not mutually exclusive — the test harness can choose at
runtime.

**A. mDNS hostname (simplest, static manifest).**
`host: "http://army-local-gateway-host.local:8080"`. Works on macOS
(Bonjour) and Linux (`systemd-resolved` with `MulticastDNS=yes`). May be
blocked in restrictive CI sandboxes.

**B. Pre-rendered LAN IP.** Harness runs `multipass info` on the gateway
after provisioning, then `envsubst` or a tiny templater rewrites the
manifest before `claws apply`. Works today, no code changes, but adds a
render step and the manifest isn't self-contained.

**C. Sentinel + D5 resolver change.** `host: "http://:8080"` resolves at
plan time using the gateway's `PublicIPv4`. Cleanest long-term; requires
the small change in D5.

Recommendation: ship **A** first (zero code), add **C** in a follow-up if
mDNS proves flaky in CI.

## Walk-through: what `claws apply -f manifests/army-local.yml` does

1. **provisioning phase** — Multipass provider `CreateInstance`s three
   VMs: `army-local-gateway-host`, `army-local-dev-host-1`,
   `army-local-qa-host-1`. Each gets ~30s first boot, <10s subsequent.
   Seeds SSH keys via cloud-init. Records LAN IPs into the plan cache.
2. **common phase** — `install_openclaw.go`, agent user setup, etc. Runs
   identically to Linode.
3. **mesh phase** — `install-caddy.Applicable` derives the scheme from
   the manifest (`ExpectedControlURLIsHTTP`) and returns false → **step
   skipped** for `http://…:8080`. `install-headscale` calls the on-demand
   `getOrResolveControlURL` helper (memoised on the plan cache) and
   binds `:8080` directly. `install-tailscale` pulls the control URL +
   preauth key via the same on-demand helpers and dials
   `--login-server=http://...:8080 --authkey=<preauth>`.
4. **channels phase** — Telegram bots, no networking changes.
5. **gateway phase** — `openclaw-gateway.service` written and enabled via
   real `systemctl`.
6. **node phase** — `bootstrap-node` runs `openclaw node install` on node
   VMs. Real `systemctl --user enable openclaw-node.service` on real
   systemd. **If issue #04 regressed, this fails here, loudly.**
7. **automations phase** — optional gvm/rust/foundry installers run the
   same way.

## Fidelity vs. Linode — honest accounting

| Aspect                        | Linode     | Multipass LAN | Matters for our bugs? |
| ----------------------------- | ---------- | ------------- | --------------------- |
| Hypervisor (KVM / virtio)     | real KVM   | KVM on Linux, Hypervisor.framework on macOS | no |
| Ubuntu cloud image            | Linode tweak of Canonical | Canonical stock | no |
| cloud-init (NoCloud)          | yes        | yes           | yes — ordering bugs   |
| systemd (system + user bus)   | yes        | yes           | yes — issue-04 class  |
| `loginctl enable-linger`      | needed     | needed        | yes                   |
| SSH bootstrap over IP         | public IP  | LAN IP        | no — same code path   |
| Headscale wire (after join)   | tailnet    | tailnet       | no — overlay identical|
| TLS on gateway                | yes (ACME) | no (HTTP)     | no — skipped by design|
| Public internet ingress       | yes        | no            | not tested here       |

What we explicitly do **not** cover: real ACME, real DNS, real cross-region
latency, real NAT traversal (DERP). Those stay on the Linode-backed
end-to-end suite if we keep one.

## Risks and mitigations

- **Multipass networking flakiness on macOS.** Mitigation: retry
  `WaitRunning` with bounded backoff; surface `multipass info` output on
  failure for postmortem.
- **mDNS blocked in CI.** Mitigation: option B (pre-render IP) is a
  one-flag fallback; or implement D5 early.
- **Snap dependency on Linux CI.** `sudo snap install multipass` requires
  snapd on the runner. `ubuntu-latest` ships with snapd; if we move to
  container-based runners later, switch to libvirt directly.
- **KVM availability in CI.** GitHub-hosted `ubuntu-latest` enabled nested
  virt / KVM in 2023. If a runner regresses, Multipass falls back to
  software emulation (very slow). Mitigation: fail fast in preflight if
  `/dev/kvm` is absent on Linux; print an explicit message.
- **Cleanup on aborted runs.** Orphaned VMs waste disk. Mitigation: test
  harness labels everything with a run-scoped prefix and always runs
  `multipass delete --all --purge` on teardown in a `t.Cleanup`.

## Milestones

1. **M1 — Provider skeleton.** `internal/hosting/multipass/` with
   `Kind()`, `CreateInstance`, `DeleteInstance`, `WaitRunning`,
   `ListByTag`. Unit tests using a fake `exec` wrapper.
2. **M2 — Schema + resolver.** `MachineTypeMultipass`, optional machine
   fields, generalized `ResolveHostedInstances`. Existing Linode tests stay
   green.
3. **M3 — Manifest + smoke test.** `manifests/army-local.yml` plus a
   minimal `integration_multipass` test that provisions one gateway and
   one node, asserts `systemctl` state, tears down.
4. **M4 — Full apply coverage.** Exercise the full `army-local.yml`
   (gateway + two nodes + agents). Assert mesh join, channels, node
   install. This is where issue-04-class bugs get caught.
5. **M5 — Optional: sentinel control URL (D5).** Only if mDNS is flaky.
6. **M6 — CI wiring.** New job in `.github/workflows/release.yml`, runs in
   parallel with the existing Docker integration job.

## Non-goals / explicit decisions

- **Keep the Docker tier.** It's fine for pure-logic tests. Not deleting
  it; not expanding it. The stub `systemctl` is now scoped to "things we
  *know* it can't test."
- **Do not run Linode tests in CI by default.** They stay as an opt-in
  nightly job (or manual `workflow_dispatch`). The Multipass tier catches
  everything except ACME/DNS/public-internet scenarios.
- **No multi-distro support in v1.** Ubuntu 24.04 only, matching the
  manifest. Adding Debian/Alma later is a libvirt-tier conversation, not a
  Multipass one.

## References

- `internal/hosting/hosting.go` — provider interface.
- `internal/hosting/linode/` — reference implementation.
- `internal/manifests/data/types.go` — machine schema.
- `internal/claws/plans/apply/mesh/control_url.go` — control URL
  derivation, already HTTP-aware.
- `internal/claws/plans/apply/mesh/install_caddy.go` — skips itself on
  HTTP control URLs.
- `internal/claws/plans/apply/provisioning/resolve.go` — post-provision IP
  resolver; needs generalization.
- `docs/issues/04-node-install-enables-gateway-unit.md` — the bug class
  this tier is explicitly designed to catch.
- `test/infra/stub-systemctl.sh`, `test/infra/Dockerfile` — what we're
  *not* using for this tier.
