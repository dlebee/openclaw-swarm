# 05 — `mesh.install-tailscale` caches installer banner as the tailnet IP

## Symptom

On a freshly provisioned machine where Tailscale is installed by the
`curl | sh` one-liner, phase `mesh.install-tailscale` completes "successfully"
but the plan cache entry for the machine's mesh IP gets set to:

```
Installing Tailscale for ubuntu noble, using method apt
```

Downstream, `node.bootstrap-node` reads that value via
`NodeTarget.GatewayInternalHost()` → `scaffold.LookupPlanMachineMeshIP()` and
splices it into the systemd unit as `--host`:

```ini
ExecStart=/usr/bin/node /usr/lib/node_modules/openclaw/dist/index.js \
  node run --host "Installing Tailscale for ubuntu noble, using method apt" \
  --port 18789 --display-name worker-node
```

The daemon starts, fails to parse the "host", exits 0 fast enough to hit
`StartLimitBurst=5` inside 60s, and the service lands in
`failed (Result: start-limit-hit)`. Because the daemon never connects back,
`node.pair-node` polls for 30s and reports:

```
pair-node: node "worker-node" did not appear as pending device after 15 attempts
```

…which wrongly looks like a pairing timing bug.

## Root cause

`internal/claws/plans/apply/mesh/install_tailscale.go` runs ONE bash script
that:

1. `sudo ufw allow 41641/udp` (silenced)
2. `curl -fsSL https://tailscale.com/install.sh | sh` on first install —
   this prints `Installing Tailscale for ubuntu noble, using method apt`
   and many other lines to stdout
3. `sudo tailscale up …`
4. `tailscale ip -4`

It then reads the combined stdout and extracted the **first non-empty
line** as the tailnet IP:

```go
for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
    if line = strings.TrimSpace(line); line != "" {
        ip = line
        break
    }
}
```

On first installs (freshly provisioned VM, Linode/Multipass, etc.) the first
non-empty line is the installer banner, not an IP. On idempotent re-runs
`curl | sh` is skipped, so the banner isn't there and the first line really
is the IP — which is why this never showed up in day-two apply runs or in
`TestMeshSmoke` (that test reads the IP directly off the VM over SSH via
`readTailscaleIP`, bypassing the plan cache).

## Fix

Parse the output as an IPv4 address and take the last valid match instead of
the first arbitrary non-empty line. `tailscale ip -4` is always the last
command in the script, so its output lines land at the bottom — walking from
the end picks the real tailnet IP even when earlier tooling spammed stdout.

```go
lines := strings.Split(strings.TrimSpace(out), "\n")
for i := len(lines) - 1; i >= 0; i-- {
    candidate := strings.TrimSpace(lines[i])
    if candidate == "" { continue }
    parsed := net.ParseIP(candidate)
    if parsed != nil && parsed.To4() != nil {
        ip = candidate
        break
    }
}
```

If no IPv4-shaped line is found we now return the full captured output in
the error, so a future regression shows up as an explicit apply failure
instead of a silent downstream unit misconfiguration.

## Why `TestMeshSmoke` didn't catch it

The mesh smoke test reads tailnet IPs directly off the VM (`tailscale ip -4`
over a fresh SSH session) and never consults `LookupPlanMachineMeshIP`. The
first end-to-end phase that does read the plan-cached value is
`node.bootstrap-node`, so the bug was latent until the first node smoke
test exercised the full mesh → gateway → node pipeline.

## Prevention

`TestNodeSmoke` (Multipass + Linode) now asserts that the node's
`openclaw-node.service` unit references the gateway's **tailnet IP**
(CGNAT 100.x range) and NOT its bridge/public IP. Any regression that
contaminates the plan-cache mesh IP will fail that assertion directly.
