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
