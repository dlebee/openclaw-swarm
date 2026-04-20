package mesh

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallTailscaleStep installs Tailscale and joins the mesh on every target machine.
type InstallTailscaleStep struct {
	dial     SSHDialFunc
	gateways []manifestdata.Gateway
	machines []manifestdata.Machine
}

func NewInstallTailscaleStep(opts Options) *InstallTailscaleStep {
	return &InstallTailscaleStep{
		dial:     opts.SSHDial,
		gateways: opts.Gateways,
		machines: opts.Machines,
	}
}

func (*InstallTailscaleStep) Name() string { return "install-tailscale" }

func (*InstallTailscaleStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*MeshTarget)
	return ok, nil
}

// Check implements scaffold.Step — asks the remote machine whether tailscale
// is installed and joined. Pure read: Check does NOT mutate the plan cache.
// Re-seeding the plan-cache mesh-IP on cold-start runs (where Execute is
// skipped because the machine is already joined) is the job of
// ResolveMeshIP, called by downstream consumers that actually need the IP.
func (s *InstallTailscaleStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		// Connection failure (including auth errors on freshly provisioned
		// machines where agent user SSH keys may not be ready yet) — treat
		// as unsatisfied so Execute gets a chance to retry with backoff.
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	ip := TailscaleIP(client)
	return ip != "", nil
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
		// Wrap in apt.WithLockRetry: the script's `apt-get update` /
		// `apt-get install tailscale` can race apt-daily on fresh
		// images. common.RunBashOutputWithRetry already handles
		// transient SSH session drops; this outer loop specifically
		// retries when the apt lock is held, which is a different
		// failure class with a much longer backoff budget.
		if err := apt.WithLockRetry(ctx, apt.RetryOpts{}, func() error {
			_, err := common.RunBashOutputWithRetry(ctx, s.dial, host, port, user, script)
			return err
		}); err != nil {
			return fmt.Errorf("install-tailscale (container) on %s: %w", m.Name, err)
		}
		return nil
	}

	// Re-derive control URL + preauth key on cache miss. In a full apply run
	// mesh-gateway seeded both cache keys before mesh-join reached this step;
	// on a cold `--only mesh-join` invocation the cache is empty and we
	// re-do the work from scratch (manifest → gateway VM disk → headscale
	// CLI if needed). The gateway MeshTarget is synthesised from the mesh
	// Options so every caller — gateway host joining its own mesh or node
	// hosts joining a remote gateway — takes the same path.
	gwMT, err := s.lookupGatewayMeshTarget(mt)
	if err != nil {
		return fmt.Errorf("install-tailscale: %w", err)
	}
	controlURL, err := getOrResolveControlURL(ctx, s.dial, gwMT)
	if err != nil {
		return fmt.Errorf("install-tailscale: %w", err)
	}
	authKey, err := getOrResolvePreauthKey(ctx, s.dial, gwMT)
	if err != nil {
		return fmt.Errorf("install-tailscale: %w", err)
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

	var out string
	if err := apt.WithLockRetry(ctx, apt.RetryOpts{}, func() error {
		o, rerr := common.RunBashOutputWithRetry(ctx, s.dial, host, port, user, script)
		out = o
		return rerr
	}); err != nil {
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
	// Tailscale bring-up in Execute reconfigures the remote's routing/NAT
	// tables and can transiently drop in-flight TCP connections, which
	// makes the immediate follow-up SSH dial from Verify race against
	// sshd rebinding. On Linode in particular the CGNAT/WireGuard
	// handshake adds tens of seconds of network churn before things
	// settle. Use the retrying dialer so Verify gives the network a
	// chance to converge instead of flipping the whole phase to failed
	// on a single post-install SSH timeout.
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
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

// lookupGatewayMeshTarget returns a *MeshTarget representing the headscale
// gateway that the caller's mesh target should join. For the gateway host
// itself the payload already carries Gateway+Machine, so we return it
// unchanged. For node hosts we scan the mesh Options for the first
// headscale gateway whose Reference points at a known machine. We assume a
// manifest has at most one headscale gateway per run — apply.BuildPlan's
// mesh phase is single-control-plane by design (see mesh.AddPhase), and
// mixing two control planes in one manifest isn't supported anywhere else
// either. If that invariant ever loosens, install_tailscale would need to
// map each target to its designated gateway instead of taking the first.
func (s *InstallTailscaleStep) lookupGatewayMeshTarget(mt *MeshTarget) (*MeshTarget, error) {
	if mt.IsGatewayHost && mt.Gateway != nil {
		return mt, nil
	}
	for i := range s.gateways {
		gw := &s.gateways[i]
		if gw.Networking == nil || !strings.EqualFold(strings.TrimSpace(gw.Networking.Mode), "headscale") {
			continue
		}
		for _, m := range s.machines {
			if m.Name == gw.Reference {
				return &MeshTarget{
					Machine:       m,
					Gateway:       gw,
					IsGatewayHost: true,
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("lookup headscale gateway for %q: none found in manifest", mt.Machine.Name)
}
