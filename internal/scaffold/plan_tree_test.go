package scaffold

import "testing"

func TestPlanSegmentsFromCells_mergesTargets(t *testing.T) {
	cells := []annotatedCell{
		{phase: "provisioning", step: "create-machine", targetID: "gw", seq: 0},
		{phase: "provisioning", step: "create-machine", targetID: "dev", seq: 1},
		{phase: "provisioning", step: "authorize-ssh-key", targetID: "gw", seq: 2},
		{phase: "provisioning", step: "authorize-ssh-key", targetID: "dev", seq: 3},
	}
	segs := planSegmentsFromCells(cells)
	if len(segs) != 1 || segs[0].phase != "provisioning" {
		t.Fatalf("phases: %+v", segs)
	}
	if len(segs[0].targets) != 2 {
		t.Fatalf("want 2 merged targets, got %d", len(segs[0].targets))
	}
	if segs[0].targets[0].id != "gw" || len(segs[0].targets[0].cells) != 2 {
		t.Fatalf("gw: %+v", segs[0].targets[0])
	}
	if segs[0].targets[1].id != "dev" || len(segs[0].targets[1].cells) != 2 {
		t.Fatalf("dev: %+v", segs[0].targets[1])
	}
}

func TestStepDisplayName(t *testing.T) {
	if g := stepDisplayName("create-machine"); g != "create machine" {
		t.Fatalf("got %q", g)
	}
	if g := stepDisplayName("authorize-ssh-key"); g != "authorized keys" {
		t.Fatalf("got %q", g)
	}
}
