package mesh

import (
	"context"
	"fmt"
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
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	return TailscaleIP(client) != "", nil
}

func (s *InstallTailscaleStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-tailscale: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

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

	var ufwExtra string
	if mt.IsGatewayHost {
		ufwExtra = `sudo ufw allow 8080/tcp comment 'headscale-control' >/dev/null 2>&1 || true
sudo ufw allow 80/tcp comment 'caddy-acme' >/dev/null 2>&1 || true
sudo ufw allow 443/tcp comment 'caddy-https' >/dev/null 2>&1 || true
`
	}

	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo ufw allow 41641/udp comment 'tailscale' >/dev/null 2>&1 || true
%sif ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi
sudo tailscale up --login-server=%q --authkey=%q --accept-dns=false
sudo ufw allow in on tailscale0 >/dev/null 2>&1 || true
tailscale ip -4
`, ufwExtra, controlURL, authKey)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("install-tailscale on %s: %w\n%s", m.Name, err, out)
	}

	ip := strings.TrimSpace(out)
	if ip == "" {
		return fmt.Errorf("install-tailscale on %s: tailscale ip -4 returned empty", m.Name)
	}
	return nil
}

func (s *InstallTailscaleStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("install-tailscale verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	if ip := TailscaleIP(client); ip == "" {
		return fmt.Errorf("install-tailscale verify: no tailscale IP on %s", m.Name)
	}
	return nil
}
