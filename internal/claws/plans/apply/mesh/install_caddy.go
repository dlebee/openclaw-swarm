package mesh

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// InstallCaddyStep installs Caddy as an HTTPS reverse proxy to Headscale on :8080.
// Not applicable when the control URL is HTTP (e.g. Docker/LAN) or for non-gateway machines.
type InstallCaddyStep struct {
	dial SSHDialFunc
}

func NewInstallCaddyStep(opts Options) *InstallCaddyStep {
	return &InstallCaddyStep{dial: opts.SSHDial}
}

func (*InstallCaddyStep) Name() string { return "install-caddy" }

func (*InstallCaddyStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MeshTarget)
	if !ok || !mt.IsGatewayHost {
		return false, nil
	}
	// Prefer the resolved control URL from the plan cache (real execute path).
	// Fall back to the manifest-derived scheme so dry-run previews reflect
	// what would happen on a real apply without requiring resolve-control-url
	// to have executed first.
	if v, ok := scaffold.PlanCacheGet(ctx, CacheKeyControlURL); ok {
		controlURL, _ := v.(string)
		return !IsHTTPControlURL(controlURL), nil
	}
	return !ExpectedControlURLIsHTTP(mt.Gateway), nil
}

func (s *InstallCaddyStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `command -v caddy >/dev/null 2>&1 && test -f /etc/caddy/Caddyfile && echo ok || echo no`)
	if err != nil || strings.TrimSpace(out) != "ok" {
		return false, nil
	}
	return true, nil
}

func (s *InstallCaddyStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-caddy: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	controlURL, err := getOrResolveControlURL(ctx, s.dial, mt)
	if err != nil {
		return fmt.Errorf("install-caddy: %w", err)
	}
	hostname := HostnameFromControlURL(controlURL)
	if hostname == "" {
		return fmt.Errorf("install-caddy: could not extract hostname from control URL %q", controlURL)
	}

	caddyfile := fmt.Sprintf("%s {\n  reverse_proxy localhost:8080\n}\n", hostname)
	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo ufw allow 80/tcp comment 'caddy-http-acme' >/dev/null 2>&1 || true
sudo ufw allow 443/tcp comment 'caddy-https' >/dev/null 2>&1 || true
if ! command -v caddy >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>/dev/null || true
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
  sudo apt-get update -qq
  sudo apt-get install -y -qq caddy
fi
sudo mkdir -p /etc/caddy
cat > /tmp/claws-Caddyfile << 'CADDYEOF'
%sCADDYEOF
sudo mv /tmp/claws-Caddyfile /etc/caddy/Caddyfile
sudo chmod 644 /etc/caddy/Caddyfile
sudo systemctl enable caddy 2>/dev/null || true
sudo systemctl reload caddy 2>/dev/null || sudo systemctl restart caddy
`, caddyfile)

	// apt.RunScriptOutput retries on apt/dpkg lock contention. The
	// `if ! command -v caddy` guard keeps the script idempotent across
	// retries (keyring writes and the apt source list are writes
	// outside the guard, but they're idempotent — same bytes every
	// run).
	out, err := apt.RunScriptOutput(ctx, client, script)
	if err != nil {
		return fmt.Errorf("install-caddy: %w\n%s", err, out)
	}
	return nil
}

func (s *InstallCaddyStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-caddy verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `command -v caddy >/dev/null 2>&1 && echo ok || echo no`)
	if err != nil || strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("install-caddy verify: caddy not installed")
	}
	return nil
}
