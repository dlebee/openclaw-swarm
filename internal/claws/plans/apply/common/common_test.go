package common

import (
	"context"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

func TestResolveMachineHost_cacheWins(t *testing.T) {
	ctx := scaffold.EnsurePlanCache(context.Background())
	m := manifestdata.Machine{Name: "gateway-host", Host: "fallback.example.com"}
	scaffold.RecordPlanMachineHost(ctx, m.Name, "203.0.113.10")
	got := ResolveMachineHost(ctx, m)
	if got != "203.0.113.10" {
		t.Fatalf("cache should win: got %q, want %q", got, "203.0.113.10")
	}
}

func TestResolveMachineHost_fallbackToSpec(t *testing.T) {
	ctx := scaffold.EnsurePlanCache(context.Background())
	m := manifestdata.Machine{Name: "static-1", Host: "static.example.com"}
	got := ResolveMachineHost(ctx, m)
	if got != "static.example.com" {
		t.Fatalf("fallback to Spec.Host: got %q", got)
	}
}

func TestResolveMachineHost_noCacheCtx(t *testing.T) {
	m := manifestdata.Machine{Name: "web", Host: "host.example.com"}
	got := ResolveMachineHost(context.Background(), m)
	if got != "host.example.com" {
		t.Fatalf("no plan-cache ctx: expected Spec.Host fallback, got %q", got)
	}
}

func TestResolveMachineHost_linodeMissingCache(t *testing.T) {
	// Linode machine with no manifest Host and no cache entry — returns
	// empty, which is the old behavior for the probe phase before
	// provisioning has resolved the instance.
	ctx := scaffold.EnsurePlanCache(context.Background())
	m := manifestdata.Machine{Name: "linode-gateway"}
	if got := ResolveMachineHost(ctx, m); got != "" {
		t.Fatalf("want empty host for unresolved Linode, got %q", got)
	}
}
