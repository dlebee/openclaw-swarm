package mesh

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ResolveControlURLStep derives the Headscale control URL from the manifest's
// public_hostname strategy and stores it in the plan cache.
// Only applicable for the gateway host machine.
type ResolveControlURLStep struct {
	dial SSHDialFunc
}

func NewResolveControlURLStep(opts Options) *ResolveControlURLStep {
	return &ResolveControlURLStep{dial: opts.SSHDial}
}

func (*ResolveControlURLStep) Name() string { return "resolve-control-url" }

func (*ResolveControlURLStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MeshTarget)
	return ok && mt.IsGatewayHost && mt.Gateway != nil, nil
}

func (s *ResolveControlURLStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	_, ok := scaffold.PlanCacheGet(ctx, CacheKeyControlURL)
	return ok, nil
}

func (s *ResolveControlURLStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	gw := mt.Gateway

	url, err := resolveControlURL(ctx, s.dial, mt, gw)
	if err != nil {
		return fmt.Errorf("resolve-control-url: %w", err)
	}
	scaffold.PlanCacheSet(ctx, CacheKeyControlURL, url)
	return nil
}

func (s *ResolveControlURLStep) Verify(ctx context.Context, _ scaffold.Target) error {
	v, ok := scaffold.PlanCacheGet(ctx, CacheKeyControlURL)
	if !ok {
		return fmt.Errorf("resolve-control-url verify: control URL not in cache")
	}
	if v.(string) == "" {
		return fmt.Errorf("resolve-control-url verify: control URL is empty")
	}
	return nil
}

// getOrResolveControlURL returns the control URL for this plan run, preferring
// the plan-cache entry seeded by resolve-control-url. On a cold start
// (`claws apply --only mesh-gateway` / `--only mesh-join` without resolve-
// control-url having run in this process) the cache is empty; we then
// re-derive the URL from the manifest's networking.public_hostname — a pure
// function of the gateway spec for strategy=custom, and an SSH "curl
// icanhazip" probe against the gateway VM for strategy=sslip. The
// re-derived value is written back to the cache so follow-up calls in the
// same run (install-headscale → install-caddy → install-tailscale) share
// one resolution instead of three.
func getOrResolveControlURL(ctx context.Context, dial SSHDialFunc, mt *MeshTarget) (string, error) {
	if v, ok := scaffold.PlanCacheGet(ctx, CacheKeyControlURL); ok {
		if s, _ := v.(string); s != "" {
			return s, nil
		}
	}
	url, err := resolveControlURL(ctx, dial, mt, mt.Gateway)
	if err != nil {
		return "", fmt.Errorf("resolve control URL: %w", err)
	}
	scaffold.PlanCacheSet(ctx, CacheKeyControlURL, url)
	return url, nil
}

func resolveControlURL(ctx context.Context, dial SSHDialFunc, mt *MeshTarget, gw *manifestdata.Gateway) (string, error) {
	if gw.Networking == nil {
		return "", fmt.Errorf("gateway %q has no networking block", gw.Name)
	}

	strategy := "sslip"
	if gw.Networking.PublicHostname != nil && gw.Networking.PublicHostname.Strategy != "" {
		strategy = strings.ToLower(strings.TrimSpace(gw.Networking.PublicHostname.Strategy))
	}

	switch strategy {
	case "sslip":
		ip, err := gatewayPublicIP(ctx, dial, mt)
		if err != nil {
			return "", err
		}
		label := SanitizeDNSLabel(gw.Name)
		dashed := strings.ReplaceAll(ip, ".", "-")
		return "https://" + label + "." + dashed + ".sslip.io", nil

	case "custom":
		if gw.Networking.PublicHostname == nil || strings.TrimSpace(gw.Networking.PublicHostname.Host) == "" {
			return "", fmt.Errorf("networking.public_hostname.host is required when strategy is custom")
		}
		h := strings.TrimSpace(gw.Networking.PublicHostname.Host)
		if strings.HasPrefix(h, "https://") || strings.HasPrefix(h, "http://") {
			return h, nil
		}
		return "https://" + h, nil

	default:
		return "", fmt.Errorf("unknown public_hostname.strategy %q", strategy)
	}
}

func gatewayPublicIP(ctx context.Context, dial SSHDialFunc, mt *MeshTarget) (string, error) {
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return "", fmt.Errorf("dial gateway for public IP: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `curl -4 -sf https://icanhazip.com 2>/dev/null || curl -4 -sf https://ifconfig.me 2>/dev/null || echo ""`)
	if err != nil {
		return "", fmt.Errorf("resolve public IP: %w", err)
	}
	ip := strings.TrimSpace(out)
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid public IP %q from gateway", ip)
	}
	return ip, nil
}
