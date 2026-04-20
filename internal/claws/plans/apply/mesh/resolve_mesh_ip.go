package mesh

import (
	"context"
	"fmt"
	"sync"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

const planCacheMeshIPResolverKey = "MESH_RESOLVE_IP_INFLIGHT"

// meshIPResolverState tracks in-flight SSH probes per machine name so
// concurrent callers deduplicate on a single provider round trip. Stored on
// the plan cache so it's shared across every target that needs the mesh IP
// during a run.
type meshIPResolverState struct {
	mu      sync.Mutex
	pending map[string]chan struct{}
}

func getOrCreateMeshIPResolver(ctx context.Context) *meshIPResolverState {
	if v, ok := scaffold.PlanCacheGet(ctx, planCacheMeshIPResolverKey); ok {
		if s, ok := v.(*meshIPResolverState); ok {
			return s
		}
	}
	s := &meshIPResolverState{pending: make(map[string]chan struct{})}
	scaffold.PlanCacheSet(ctx, planCacheMeshIPResolverKey, s)
	return s
}

// ResolveMeshIP returns the tailscale IPv4 for machine m, consulting the
// plan-cache memo first and falling back to an SSH probe.
//
// Read-through semantics: cold-cache and hot-cache produce the same answer,
// so callers get the right IP regardless of whether mesh.install-tailscale
// has already populated it in this plan run.
//
// Concurrent callers for the same machine are deduplicated: the second
// caller waits on the first's SSH session instead of opening a parallel one.
//
// MUST NOT be called from Check/Applicable. The SSH probe is an observable
// side effect that belongs to the probe lifecycle (Execute/Verify), not to
// the purely-read predicate layer.
func ResolveMeshIP(ctx context.Context, dial common.SSHDialFunc, m manifestdata.Machine) (string, error) {
	if ip, ok := scaffold.LookupPlanMachineMeshIP(ctx, m.Name); ok && ip != "" {
		return ip, nil
	}
	state := getOrCreateMeshIPResolver(ctx)
	for {
		state.mu.Lock()
		if ip, ok := scaffold.LookupPlanMachineMeshIP(ctx, m.Name); ok && ip != "" {
			state.mu.Unlock()
			return ip, nil
		}
		if ch, inflight := state.pending[m.Name]; inflight {
			state.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			continue
		}
		done := make(chan struct{})
		state.pending[m.Name] = done
		state.mu.Unlock()

		ip, err := probeMeshIP(ctx, dial, m)

		state.mu.Lock()
		delete(state.pending, m.Name)
		state.mu.Unlock()
		if err == nil && ip != "" {
			scaffold.RecordPlanMachineMeshIP(ctx, m.Name, ip)
		}
		close(done)
		if err != nil {
			return "", err
		}
		return ip, nil
	}
}

func probeMeshIP(ctx context.Context, dial common.SSHDialFunc, m manifestdata.Machine) (string, error) {
	if dial == nil {
		return "", fmt.Errorf("resolve-mesh-ip: no SSH dialer for %q", m.Name)
	}
	host := common.ResolveMachineHost(ctx, m)
	if host == "" {
		return "", fmt.Errorf("resolve-mesh-ip: no host for %q", m.Name)
	}
	client, key, err := common.BorrowSSH(ctx, dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return "", fmt.Errorf("resolve-mesh-ip: dial %q: %w", m.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)
	ip := TailscaleIP(client)
	if ip == "" {
		return "", fmt.Errorf("resolve-mesh-ip: no tailscale IPv4 on %q", m.Name)
	}
	return ip, nil
}
