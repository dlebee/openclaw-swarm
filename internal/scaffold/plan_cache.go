package scaffold

import (
	"context"
	"fmt"
	"strings"
)

type planCacheCtxKey struct{}

// WithPlanCache attaches a mutable map to ctx for plan-scoped data (probe and/or execute).
// The same map can be shared across Applicable/Check/Execute for all cells in one run.
func WithPlanCache(ctx context.Context, data map[string]any) context.Context {
	if data == nil {
		data = make(map[string]any)
	}
	return context.WithValue(ctx, planCacheCtxKey{}, data)
}

// EnsurePlanCache returns ctx unchanged if a plan cache is already present; otherwise attaches a new empty map.
func EnsurePlanCache(ctx context.Context) context.Context {
	if _, ok := PlanCache(ctx); ok {
		return ctx
	}
	return WithPlanCache(ctx, make(map[string]any))
}

// PlanCache returns the map attached with [WithPlanCache] / [EnsurePlanCache], if any.
func PlanCache(ctx context.Context) (map[string]any, bool) {
	m, ok := ctx.Value(planCacheCtxKey{}).(map[string]any)
	return m, ok
}

// PlanCacheSet assigns key in the plan cache. It is a no-op if ctx has no cache.
func PlanCacheSet(ctx context.Context, key string, value any) {
	m, ok := PlanCache(ctx)
	if !ok {
		return
	}
	m[key] = value
}

// PlanCacheGet returns a cache entry. The bool is false if missing or if ctx has no cache.
func PlanCacheGet(ctx context.Context, key string) (any, bool) {
	m, ok := PlanCache(ctx)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
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
