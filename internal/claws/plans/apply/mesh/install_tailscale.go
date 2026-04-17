package mesh

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallTailscaleStep installs Tailscale and joins the mesh on every target machine.
type InstallTailscaleStep struct {
	dial SSHDialFunc
}

func NewInstallTailscaleStep(opts Options) *InstallTailscaleStep {
	return &InstallTailscaleStep{dial: opts.SSHDial}
}

func (*InstallTailscaleStep) Name() string { return "install-tailscale" }

func (*InstallTailscaleStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*MeshTarget)
	return ok, nil
}

func (s *InstallTailscaleStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	ip := TailscaleIP(client)
	if ip == "" {
		return false, nil
	}
	// Re-populate the plan cache on idempotent runs where Execute is skipped
	// because tailscale is already joined. Downstream phases (node
	// bootstrap) depend on this being set.
	scaffold.RecordPlanMachineMeshIP(ctx, m.Name, ip)
	return true, nil
}

func (s *InstallTailscaleStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	host, port, user := common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m)

	// In container (Docker) environments, kernel TUN is unavailable so we cannot
	// run `tailscale up`. Install the binary only and skip the join step.
	if mt.Machine.Container {
		script := `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v tailscale >/dev/null 2>&1; then
  mkdir -p /usr/share/keyrings
  curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg | sudo tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
  curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list | sudo tee /etc/apt/sources.list.d/tailscale.list >/dev/null
  sudo apt-get update -qq
  sudo apt-get install -y -qq tailscale
fi
echo "container-skip-join"
`
		if _, err := common.RunBashOutputWithRetry(ctx, s.dial, host, port, user, script); err != nil {
			return fmt.Errorf("install-tailscale (container) on %s: %w", m.Name, err)
		}
		return nil
	}

	v, _ := scaffold.PlanCacheGet(ctx, CacheKeyControlURL)
	controlURL, _ := v.(string)
	if controlURL == "" {
		return fmt.Errorf("install-tailscale: control URL not resolved")
	}
	v, _ = scaffold.PlanCacheGet(ctx, CacheKeyPreauthKey)
	authKey, _ := v.(string)
	if authKey == "" {
		return fmt.Errorf("install-tailscale: preauth key not resolved")
	}
	// Strip scheme/port to get the bare hostname we'll seed into /etc/hosts
	// below. HostnameFromControlURL handles http://host:port, https://host/,
	// and raw host:port forms uniformly.
	controlHost := HostnameFromControlURL(controlURL)

	var ufwExtra string
	if mt.IsGatewayHost {
		ufwExtra = `sudo ufw allow 8080/tcp comment 'headscale-control' >/dev/null 2>&1 || true
sudo ufw allow 80/tcp comment 'caddy-acme' >/dev/null 2>&1 || true
sudo ufw allow 443/tcp comment 'caddy-https' >/dev/null 2>&1 || true
`
	}

	// tailscaled is a static Go binary built with netgo — its resolver
	// reads /etc/resolv.conf and /etc/hosts directly but does NOT consult
	// libnss_* plugins. That matters when the control URL is an mDNS name
	// like `gateway-host.local`: libc-backed tools (curl, ssh, getent)
	// resolve it fine through nss_mdns, but tailscaled sees "no DNS
	// fallback candidates remain" and blocks `tailscale up` forever.
	//
	// Workaround: before `tailscale up`, pin the control-URL host in
	// /etc/hosts using whatever NSS can currently resolve. `getent hosts`
	// hits the same search chain libc does (files → mdns → dns), so it
	// picks up the Avahi announcement even though tailscaled can't.
	//
	//   - If getent finds nothing, the script leaves /etc/hosts alone and
	//     `tailscale up` will fall back to its usual DNS path. On Linode
	//     the hostname is already in public DNS, so that's fine.
	//   - If /etc/hosts already has a line for this host, we leave it:
	//     production hostnames may be present from user-managed /etc/hosts,
	//     and we don't want to shadow them.
	//   - The grep pattern is anchored to tab/space-separated tokens so we
	//     don't false-positive on `gateway-host` when looking for
	//     `gateway-host.local`.
	pinHostsSnippet := fmt.Sprintf(`
CTRL_HOST=%q
if [ -n "$CTRL_HOST" ] && ! grep -qE "[[:space:]]${CTRL_HOST}([[:space:]]|$)" /etc/hosts; then
  IP="$(getent hosts "$CTRL_HOST" 2>/dev/null | awk '{print $1}' | head -n1 || true)"
  if [ -n "$IP" ]; then
    echo "$IP $CTRL_HOST" | sudo tee -a /etc/hosts >/dev/null
  fi
fi
`, controlHost)

	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo ufw allow 41641/udp comment 'tailscale' >/dev/null 2>&1 || true
%s%sif ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi
# Ensure tailscaled is running before calling tailscale up.
if ! sudo systemctl is-active --quiet tailscaled 2>/dev/null; then
  sudo systemctl start tailscaled 2>/dev/null || true
fi
# Wait for tailscaled socket (path varies by mode).
for sock in /run/tailscale/tailscaled.sock /var/run/tailscale/tailscaled.sock; do
  i=0
  while [ $i -lt 30 ]; do
    [ -S "$sock" ] && break
    sleep 1; i=$((i+1))
  done
  [ -S "$sock" ] && break
done
sudo tailscale up --login-server=%q --authkey=%q --accept-dns=false
sudo ufw allow in on tailscale0 >/dev/null 2>&1 || true
tailscale ip -4
`, ufwExtra, pinHostsSnippet, controlURL, authKey)

	out, err := common.RunBashOutputWithRetry(ctx, s.dial, host, port, user, script)
	if err != nil {
		return fmt.Errorf("install-tailscale on %s: %w", m.Name, err)
	}

	// The bash script above emits earlier tooling output (ufw, curl | sh
	// installer banner, tailscale up progress) BEFORE the final
	// `tailscale ip -4` line, so we can't just take the first line. Walk
	// the output in reverse and pick the last line that parses as a valid
	// IPv4 address — that's the tailnet address the installer printed
	// last. This also tolerates multi-IP nodes: the first tailnet IP is
	// the canonical one and gets printed last by `tailscale ip -4` as the
	// bottom-most address.
	var ip string
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		if candidate == "" {
			continue
		}
		parsed := net.ParseIP(candidate)
		if parsed != nil && parsed.To4() != nil {
			ip = candidate
			break
		}
	}
	if ip == "" {
		return fmt.Errorf("install-tailscale on %s: no IPv4 address in output:\n%s", m.Name, out)
	}

	// Expose the mesh-local IP to downstream phases (node.bootstrap-node uses
	// this to tell the node daemon which gateway address to dial). The
	// tailnet address is both routable between mesh peers AND accepted as
	// "private" by openclaw's security check that rejects plaintext ws:// to
	// public IPs — bootstrap-node would otherwise install a unit that
	// refuses to start with "SECURITY ERROR: Cannot connect over plaintext
	// ws://".
	scaffold.RecordPlanMachineMeshIP(ctx, m.Name, ip)
	return nil
}

func (s *InstallTailscaleStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-tailscale verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// In container mode we only install the binary, not join — verify binary exists.
	if mt.Machine.Container {
		out, err := bash.RunOutput(client, `command -v tailscale >/dev/null 2>&1 && echo ok || echo missing`)
		if err != nil || strings.TrimSpace(out) != "ok" {
			return fmt.Errorf("install-tailscale verify: tailscale binary missing on %s", m.Name)
		}
		return nil
	}

	if ip := TailscaleIP(client); ip == "" {
		return fmt.Errorf("install-tailscale verify: no tailscale IP on %s", m.Name)
	}
	return nil
}
