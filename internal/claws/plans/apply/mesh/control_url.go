package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// errGatewayNotProvisioned signals that the headscale gateway VM does
// not yet have a reachable host (cache + manifest + resolver all empty),
// i.e. provisioning.create-machine hasn't Execute'd yet in this plan
// run. Check-side callers treat this as a "will execute" signal rather
// than a failed probe; the sslip control-URL derivation fundamentally
// needs the gateway's public IP, and there's no way to obtain that
// without dialing the VM.
var errGatewayNotProvisioned = errors.New("gateway machine not provisioned yet")

// getOrResolveControlURL returns the control URL for this plan run. The
// value is memoised on the plan cache so the first caller in a run pays
// for the resolution and the rest hit the in-memory cache; callers are
// install-headscale, install-caddy, and install-tailscale.
//
// Resolution is an on-demand helper rather than a dedicated plan step:
// a step whose only job is to populate an in-memory cache would always
// render "will execute" in the plan tree even on a fully-converged
// system, which is noise. The derivation itself is a pure function of
// the gateway spec for strategy=custom, and an SSH "curl icanhazip"
// probe against the gateway VM for strategy=sslip.
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
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		// Gateway machine not provisioned yet. Return a typed "not
		// ready" signal that resolveControlURL callers can treat as
		// "defer until Execute". Used by the probe to avoid rendering
		// a confusing dial-error for a machine that provisioning
		// will create later in this plan.
		return "", errGatewayNotProvisioned
	}
	client, key, err := common.BorrowSSH(ctx, dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
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
