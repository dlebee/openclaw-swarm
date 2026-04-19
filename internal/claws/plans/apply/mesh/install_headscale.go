package mesh

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"gopkg.in/yaml.v3"
)

// InstallHeadscaleStep installs the embedded Headscale server on the gateway
// reference machine: binary from GitHub, config, systemd unit, default user.
type InstallHeadscaleStep struct {
	dial SSHDialFunc
}

func NewInstallHeadscaleStep(opts Options) *InstallHeadscaleStep {
	return &InstallHeadscaleStep{dial: opts.SSHDial}
}

func (*InstallHeadscaleStep) Name() string { return "install-headscale" }

func (*InstallHeadscaleStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MeshTarget)
	return ok && mt.IsGatewayHost, nil
}

func (s *InstallHeadscaleStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	return HeadscaleInstalled(client) && HeadscaleVersionOK(client), nil
}

func (s *InstallHeadscaleStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-headscale: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	v, _ := scaffold.PlanCacheGet(ctx, CacheKeyControlURL)
	controlURL, _ := v.(string)
	if controlURL == "" {
		return fmt.Errorf("install-headscale: control URL not resolved")
	}

	cfgYAML, err := headscaleConfigYAML(controlURL)
	if err != nil {
		return fmt.Errorf("install-headscale: build config: %w", err)
	}
	aclJSON := `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}` + "\n"

	ver := headscaleVersion
	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo mkdir -p /var/lib/headscale /var/run/headscale /etc/headscale /var/lib/claws/headscale
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) HS_ARCH=amd64 ;;
  aarch64) HS_ARCH=arm64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
# Download only if headscale is not already installed at the right version.
if ! /usr/local/bin/headscale version 2>/dev/null | grep -qF "%s"; then
  URL="https://github.com/juanfont/headscale/releases/download/v%s/headscale_%s_linux_${HS_ARCH}"
  sudo curl -fsSL "$URL" -o /usr/local/bin/headscale
  sudo chmod +x /usr/local/bin/headscale
fi

cat > /tmp/claws-headscale-config.yaml << 'HSCONFIG'
%sHSCONFIG
sudo mv /tmp/claws-headscale-config.yaml /etc/headscale/config.yaml

cat > /tmp/claws-headscale-acl.hujson << 'HSACL'
%sHSACL
sudo mv /tmp/claws-headscale-acl.hujson /etc/headscale/acl.hujson
sudo chmod 644 /etc/headscale/config.yaml /etc/headscale/acl.hujson

sudo ufw allow 8080/tcp comment 'headscale-control' >/dev/null 2>&1 || true

sudo tee /etc/systemd/system/headscale.service >/dev/null <<'UNIT'
[Unit]
Description=Headscale coordination server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/headscale serve --config /etc/headscale/config.yaml
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable headscale
sudo systemctl restart headscale

i=1
while [ "$i" -le 90 ]; do
  if sudo test -S /var/run/headscale/headscale.sock; then break; fi
  sleep 1
  i=$((i+1))
done
if ! sudo test -S /var/run/headscale/headscale.sock; then
  echo "headscale unix socket did not appear after 90s" >&2
  sudo journalctl -u headscale -n 60 --no-pager >&2 || true
  exit 1
fi

sudo /usr/local/bin/headscale users create default 2>/dev/null || true
`, ver, ver, ver, string(cfgYAML), aclJSON)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("install-headscale: %w\n%s", err, out)
	}
	return nil
}

func (s *InstallHeadscaleStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-headscale verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	if !HeadscaleInstalled(client) {
		return fmt.Errorf("install-headscale verify: binary or config missing")
	}

	out, err := bash.RunOutput(client, `sudo test -S /var/run/headscale/headscale.sock && echo ok || echo no`)
	if err != nil || strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("install-headscale verify: unix socket not present")
	}
	return nil
}

func headscaleConfigYAML(serverURL string) ([]byte, error) {
	cfg := map[string]interface{}{
		"server_url":          serverURL,
		"listen_addr":         "0.0.0.0:8080",
		"metrics_listen_addr": "127.0.0.1:9090",
		"grpc_listen_addr":    "127.0.0.1:50443",
		"grpc_allow_insecure": true,
		"noise": map[string]interface{}{
			"private_key_path": "/var/lib/headscale/noise_private.key",
		},
		"prefixes": map[string]interface{}{
			"v4":         "100.64.0.0/10",
			"v6":         "fd7a:115c:a1e0::/48",
			"allocation": "sequential",
		},
		"derp": map[string]interface{}{
			"server": map[string]interface{}{
				"enabled": false,
			},
			"urls": []string{"https://controlplane.tailscale.com/derpmap/default"},
		},
		"database": map[string]interface{}{
			"type": "sqlite",
			"sqlite": map[string]interface{}{
				"path":            "/var/lib/headscale/db.sqlite",
				"write_ahead_log": true,
			},
		},
		"log": map[string]interface{}{
			"level":  "info",
			"format": "text",
		},
		"policy": map[string]interface{}{
			"mode": "file",
			"path": "/etc/headscale/acl.hujson",
		},
		"dns": map[string]interface{}{
			"magic_dns":          true,
			"base_domain":        "mesh.claws.internal",
			"override_local_dns": true,
			"nameservers": map[string]interface{}{
				"global": []string{"1.1.1.1", "1.0.0.1"},
			},
		},
		"unix_socket":            "/var/run/headscale/headscale.sock",
		"unix_socket_permission": "0770",
	}
	return yaml.Marshal(cfg)
}
