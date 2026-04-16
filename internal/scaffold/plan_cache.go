package scaffold

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

type planCacheCtxKey struct{}

// planCache wraps the data map with a mutex for safe concurrent access and
// tracks io.Closer resources that are cleaned up when the plan finishes.
type planCache struct {
	mu       sync.Mutex
	data     map[string]any
	closers  []io.Closer
}

func newPlanCache() *planCache {
	return &planCache{data: make(map[string]any)}
}

// WithPlanCache attaches a [planCache] to ctx for plan-scoped data (probe
// and/or execute). The same cache is shared across all cells in one run.
// Deprecated: prefer [EnsurePlanCache] which creates one if absent.
func WithPlanCache(ctx context.Context, data map[string]any) context.Context {
	pc := newPlanCache()
	for k, v := range data {
		pc.data[k] = v
	}
	return context.WithValue(ctx, planCacheCtxKey{}, pc)
}

// EnsurePlanCache returns ctx unchanged if a plan cache is already present;
// otherwise attaches a new empty cache.
func EnsurePlanCache(ctx context.Context) context.Context {
	if pc := getPlanCache(ctx); pc != nil {
		return ctx
	}
	return context.WithValue(ctx, planCacheCtxKey{}, newPlanCache())
}

func getPlanCache(ctx context.Context) *planCache {
	pc, _ := ctx.Value(planCacheCtxKey{}).(*planCache)
	return pc
}

// PlanCache returns the data map for backward compatibility. Callers that need
// concurrency safety should use PlanCacheGet / PlanCacheSet instead.
func PlanCache(ctx context.Context) (map[string]any, bool) {
	pc := getPlanCache(ctx)
	if pc == nil {
		return nil, false
	}
	return pc.data, true
}

// PlanCacheSet assigns key in the plan cache. It is a no-op if ctx has no cache.
func PlanCacheSet(ctx context.Context, key string, value any) {
	pc := getPlanCache(ctx)
	if pc == nil {
		return
	}
	pc.mu.Lock()
	pc.data[key] = value
	pc.mu.Unlock()
}

// PlanCacheGet returns a cache entry. The bool is false if missing or if ctx has no cache.
func PlanCacheGet(ctx context.Context, key string) (any, bool) {
	pc := getPlanCache(ctx)
	if pc == nil {
		return nil, false
	}
	pc.mu.Lock()
	v, ok := pc.data[key]
	pc.mu.Unlock()
	return v, ok
}

// PlanCacheBool reads a bool entry. The second bool is false if missing or not a bool.
func PlanCacheBool(ctx context.Context, key string) (bool, bool) {
	v, ok := PlanCacheGet(ctx, key)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// RegisterPlanCloser adds an io.Closer that will be closed by [ClosePlanResources].
// Typically used for connection pools or other plan-scoped resources.
func RegisterPlanCloser(ctx context.Context, c io.Closer) {
	pc := getPlanCache(ctx)
	if pc == nil {
		return
	}
	pc.mu.Lock()
	pc.closers = append(pc.closers, c)
	pc.mu.Unlock()
}

// ClosePlanResources closes all io.Closers registered with [RegisterPlanCloser].
// Call once when the plan run finishes.
func ClosePlanResources(ctx context.Context) {
	pc := getPlanCache(ctx)
	if pc == nil {
		return
	}
	pc.mu.Lock()
	closers := pc.closers
	pc.closers = nil
	pc.mu.Unlock()
	for _, c := range closers {
		c.Close()
	}
}

func planCacheMachineExistsKey(targetID string) string {
	key := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(targetID), "-", "_"))
	if key == "" {
		key = "UNKNOWN"
	}
	return fmt.Sprintf("MACHINE_%s_EXISTS", key)
}

// RecordPlanMachineExists stores the result of a create-machine (or equivalent) probe for
// this plan run. Other code must use [DoesMachineExist]; do not read the plan cache by key.
func RecordPlanMachineExists(ctx context.Context, targetID string, exists bool) {
	PlanCacheSet(ctx, planCacheMachineExistsKey(targetID), exists)
}

// DoesMachineExist reports whether this plan run has a definitive answer for “instance
// already present” for the given target (typically from create-machine Check).
//
// If known is false, callers must not treat the machine as present or absent—for example
// when that phase was skipped or has not run yet on this context.
func DoesMachineExist(ctx context.Context, targetID string) (exists bool, known bool) {
	return PlanCacheBool(ctx, planCacheMachineExistsKey(targetID))
}
