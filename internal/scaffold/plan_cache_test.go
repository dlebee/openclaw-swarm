package scaffold

import (
	"context"
	"testing"
)

func TestDoesMachineExist_unknown(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	_, known := DoesMachineExist(ctx, "web")
	if known {
		t.Fatal("want unknown before RecordPlanMachineExists")
	}
}

func TestDoesMachineExist_afterRecord(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	id := "gateway-host"
	RecordPlanMachineExists(ctx, id, true)
	exists, known := DoesMachineExist(ctx, id)
	if !known || !exists {
		t.Fatalf("want exists=true known=true, got exists=%v known=%v", exists, known)
	}
	RecordPlanMachineExists(ctx, id, false)
	exists, known = DoesMachineExist(ctx, id)
	if !known || exists {
		t.Fatalf("after overwrite want exists=false known=true, got exists=%v known=%v", exists, known)
	}
}

func TestEnsurePlanCache_genericKey(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	PlanCacheSet(ctx, "OTHER", 1)
	v, ok := PlanCacheGet(ctx, "OTHER")
	if !ok || v != 1 {
		t.Fatalf("generic cache: ok=%v v=%v", ok, v)
	}
}

func TestLookupPlanMachineHost_missing(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	if h, ok := LookupPlanMachineHost(ctx, "web"); ok || h != "" {
		t.Fatalf("want ok=false empty, got ok=%v h=%q", ok, h)
	}
}

func TestRecordPlanMachineHost_roundTrip(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	RecordPlanMachineHost(ctx, "gateway-host", "203.0.113.10")
	h, ok := LookupPlanMachineHost(ctx, "gateway-host")
	if !ok || h != "203.0.113.10" {
		t.Fatalf("round trip: ok=%v h=%q", ok, h)
	}
	// Same name, different casing / dashes — normalization should hit the
	// same cache slot.
	if h, ok := LookupPlanMachineHost(ctx, "Gateway_Host"); !ok || h != "203.0.113.10" {
		t.Fatalf("normalized lookup: ok=%v h=%q", ok, h)
	}
}

func TestRecordPlanMachineHost_emptyClears(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	RecordPlanMachineHost(ctx, "web", "1.2.3.4")
	RecordPlanMachineHost(ctx, "web", "  ")
	if _, ok := LookupPlanMachineHost(ctx, "web"); ok {
		t.Fatal("empty host should have cleared the cache entry")
	}
}

func TestForgetPlanMachineHost(t *testing.T) {
	ctx := EnsurePlanCache(context.Background())
	RecordPlanMachineHost(ctx, "web", "1.2.3.4")
	ForgetPlanMachineHost(ctx, "web")
	if _, ok := LookupPlanMachineHost(ctx, "web"); ok {
		t.Fatal("want Forget to remove the entry")
	}
	// Forgetting without a cache or an entry is a no-op.
	ForgetPlanMachineHost(context.Background(), "web")
}

func TestPlanMachineHost_noCacheCtx(t *testing.T) {
	// Calls against a ctx with no cache must no-op silently, same as the
	// existing PlanCacheSet/Get contract.
	RecordPlanMachineHost(context.Background(), "web", "1.2.3.4")
	if _, ok := LookupPlanMachineHost(context.Background(), "web"); ok {
		t.Fatal("lookup on bare ctx should return ok=false")
	}
}
