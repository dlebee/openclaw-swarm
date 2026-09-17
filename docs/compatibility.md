# OpenClaw compatibility

Which OpenClaw releases each `claws` tag was verified against, and which Node.js
line it installs on managed machines.

"Verified" means the Linode integration tier (`test/integration/linode`, 10
tests) was run green against that OpenClaw version at that commit — not merely
that it is expected to work.

| claws tag | Date | OpenClaw verified | Node installed | Notes |
| --------- | ---- | ----------------- | -------------- | ----- |
| [v0.0.13](https://github.com/dlebee/openclaw-swarm/releases/tag/v0.0.13) | 2026-09-16 | 2026.7.1, 2026.8.2, 2026.9.3, 2026.9.4 | 24 (`node_major` override) | Node 24 default; `install-nodejs` enforces OpenClaw's engine floor. 2026.9.x requires `node >=24.16.0 <25 \|\| >=26.1.0`. |
| [v0.0.12](https://github.com/dlebee/openclaw-swarm/releases/tag/v0.0.12) | 2026-09-02 | 2026.7.1, 2026.8.1 | 22 | Roster reshape (`agents.list` → `agents.entries`) and the systemd install-identity gate. Last tag before Node 24; **cannot install 2026.9.x** (EBADENGINE). |
| [v0.0.11](https://github.com/dlebee/openclaw-swarm/releases/tag/v0.0.11) | 2026-08-18 | 2026.7.1 | 22 | Single pinned version (`OPENCLAW_VERSION` default), no matrix. |
| [v0.0.10](https://github.com/dlebee/openclaw-swarm/releases/tag/v0.0.10) | 2026-08-17 | 2026.7.1 | 22 | First tag with a pinned OpenClaw version in CI. |
| v0.0.1 – v0.0.9 | 2026-04-19 → 2026-05-24 | not recorded | 22 | CI installed npm `latest` at run time; no version was pinned, so there is no reliable record of what was exercised. |

## Current default matrix

`.github/workflows/integration-linode.yml` runs these by default, one leg at a
time (concurrent legs trip Linode's instance-creation rate limit):

```
2026.9.4, 2026.8.2, 2026.7.1
```

One release per supported OpenClaw line. A `workflow_dispatch` run can pin any
single version via the `openclaw_version` input, and `test_filter` narrows it to
one test (handy for re-running a leg that failed on cloud flakiness rather than
on a real defect).

## Node.js floors

`install-nodejs` installs `common.DefaultNodeMajor` (24) unless a manifest sets
`node_major`, and re-runs whenever a host is on a different major or below the
floor for its line. Floors mirror OpenClaw's `package.json` engines:

| Node line | Floor | Required by |
| --------- | ----- | ----------- |
| 22 | 22.22.3 | OpenClaw ≤ 2026.8.x |
| 24 | 24.16.0 | OpenClaw 2026.9.x |
| 26 | 26.1.0 | OpenClaw 2026.9.x |

apt will not downgrade, so moving a host to an *older* major means removing
`nodejs` first.

## Keeping this current

Update the table when you cut a tag, and when you change the default matrix in
`.github/workflows/integration-linode.yml`. Only list a version under "verified"
once a green run exists for it — a link to the run in the PR is enough.

To check the newest release before bumping:

```bash
npm view openclaw dist-tags          # latest / beta / extended-stable
npm view openclaw@<version> engines.node
```

If the engine range moves outside the current `DefaultNodeMajor`, bumping the
matrix is not enough — `install-nodejs` needs the new major too.
