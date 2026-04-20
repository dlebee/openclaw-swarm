package provisioning

import (
	"context"
	"fmt"
	"sync"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// MachineStatus is the plan-scoped snapshot of a single machine target.
// Once populated, callers read Exists and Instance without another provider
// round trip for the rest of the plan run.
//
// Instance is non-nil iff the provider reported exactly one matching
// instance. Callers MUST treat the pointed-to Instance as read-only: the
// resolver memoizes a snapshot and will not observe mutations applied by
// callers.
type MachineStatus struct {
	Exists   bool
	Instance *hosting.Instance
}

const planCacheMachineStatusKey = "PROVISIONING_MACHINE_STATUS_CACHE"

// machineStatusCache memoizes ResolveMachineStatus verdicts for one plan
// run. Stored on the plan cache so the same resolver state is shared across
// every Check and Execute in the run without leaking between runs.
//
// pending[key] lets concurrent resolvers for the same machine deduplicate:
// the second caller waits on the first's broadcast channel instead of
// issuing a parallel ListByTag against the hosting provider.
type machineStatusCache struct {
	mu      sync.Mutex
	data    map[string]MachineStatus
	pending map[string]chan struct{}
}

func getOrCreateMachineStatusCache(ctx context.Context) *machineStatusCache {
	if v, ok := scaffold.PlanCacheGet(ctx, planCacheMachineStatusKey); ok {
		if c, ok := v.(*machineStatusCache); ok {
			return c
		}
	}
	c := &machineStatusCache{
		data:    make(map[string]MachineStatus),
		pending: make(map[string]chan struct{}),
	}
	scaffold.PlanCacheSet(ctx, planCacheMachineStatusKey, c)
	return c
}

// ResolveMachineStatus returns the current provisioning status for mt,
// using a plan-scoped cache as a pure latency optimization: a cold cache
// and a hot cache must produce the same verdict.
//
// Concurrent callers for the same (prefix, machine) are deduplicated —
// the second caller waits on the first's ListByTag instead of issuing a
// parallel provider call.
//
// This function is the single source of truth for the "does this machine
// exist?" question. Both Check and Execute in create_machine call it;
// downstream phases call it too when they need the *hosting.Instance
// without depending on a previous Check having mutated the payload.
//
// The returned MachineStatus is a snapshot; callers must not mutate
// Instance fields.
func ResolveMachineStatus(ctx context.Context, provider hosting.Provider, prefix string, mt *MachineTarget) (MachineStatus, error) {
	if mt == nil {
		return MachineStatus{}, fmt.Errorf("resolve-machine-status: nil target")
	}
	if provider == nil {
		return MachineStatus{}, fmt.Errorf("resolve-machine-status: nil provider for %q", mt.Spec.Name)
	}
	cache := getOrCreateMachineStatusCache(ctx)
	key := machineStatusCacheKey(prefix, mt.Spec.Name)

	for {
		cache.mu.Lock()
		if v, ok := cache.data[key]; ok {
			cache.mu.Unlock()
			return v, nil
		}
		if ch, inflight := cache.pending[key]; inflight {
			cache.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return MachineStatus{}, ctx.Err()
			}
			continue
		}
		done := make(chan struct{})
		cache.pending[key] = done
		cache.mu.Unlock()

		status, err := lookupMachineStatus(ctx, provider, prefix, mt.Spec.Name)

		cache.mu.Lock()
		delete(cache.pending, key)
		if err == nil {
			cache.data[key] = status
		}
		cache.mu.Unlock()
		close(done)
		if err != nil {
			return MachineStatus{}, err
		}
		return status, nil
	}
}

// RecordMachineStatus seeds the resolver cache with a known-good status.
// Intended for Execute paths that just created or modified an instance
// and already hold the authoritative data; saves subsequent Resolve
// callers a provider round trip.
//
// Passing a zero MachineStatus records "does not exist" explicitly.
func RecordMachineStatus(ctx context.Context, prefix, machineName string, status MachineStatus) {
	cache := getOrCreateMachineStatusCache(ctx)
	key := machineStatusCacheKey(prefix, machineName)
	cache.mu.Lock()
	cache.data[key] = status
	cache.mu.Unlock()
}

// InvalidateMachineStatus removes any cached verdict for the given
// machine. Call from destroy flows after an instance has been torn down
// so subsequent Resolve calls in the same process re-probe the world.
func InvalidateMachineStatus(ctx context.Context, prefix, machineName string) {
	cache := getOrCreateMachineStatusCache(ctx)
	key := machineStatusCacheKey(prefix, machineName)
	cache.mu.Lock()
	delete(cache.data, key)
	cache.mu.Unlock()
}

func lookupMachineStatus(ctx context.Context, provider hosting.Provider, prefix, machineName string) (MachineStatus, error) {
	prefixTag := clawsPrefixTag(prefix)
	wantLabel := machineLabel(prefix, machineName)
	instances, err := provider.ListByTag(ctx, prefixTag)
	if err != nil {
		return MachineStatus{}, err
	}
	var matches []hosting.Instance
	for i := range instances {
		if instances[i].Label == wantLabel {
			matches = append(matches, instances[i])
		}
	}
	switch len(matches) {
	case 0:
		return MachineStatus{Exists: false}, nil
	case 1:
		inst := matches[0]
		return MachineStatus{Exists: true, Instance: &inst}, nil
	default:
		return MachineStatus{}, fmt.Errorf("resolve-machine-status: %d instances with label %q under tag %q (want at most 1)",
			len(matches), wantLabel, prefixTag)
	}
}

func machineStatusCacheKey(prefix, machineName string) string {
	return prefix + "/" + machineName
}
