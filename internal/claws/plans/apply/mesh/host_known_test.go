package mesh_test

// Regression tests for the "probe UI shows 'check: dial :22: connection
// refused' for un-provisioned machines" class of bug.
//
// Background: in a cold-start `claws apply`, provisioning.create-machine
// hasn't Execute'd yet at probe time, so the hosting provider hasn't
// minted a VM, the plan cache is empty, and ResolveMachineHost returns
// "". Before this guard existed, every Check in mesh/gateway/node/etc.
// would happily call BorrowSSH("", 22, ...) which bottoms out at
// net.Dial with ":22" (resolves to localhost port 22) and yields the
// confusing
//
//	check: dial gateway-host: dial tcp :22: connect: connection refused
//
// noise for every phase after provisioning. The probe honestly has no
// signal to work with — the machine doesn't exist yet — so the
// correct verdict is "not satisfied, will execute" (Check → false,
// nil) and the dial must be skipped.

import (
	"context"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/mesh"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// A dialer that always panics; if Check reaches it, the guard failed.
func panicDialer(_ context.Context, _ string, _ int, _ string) (*xssh.Client, error) {
	panic("BorrowSSH should never be called when the machine is un-provisioned")
}

func TestInstallTailscaleCheck_unprovisionedShortCircuits(t *testing.T) {
	// No plan-cache host, no manifest Host, no resolver: the machine
	// has not been created yet.
	ctx := scaffold.EnsurePlanCache(context.Background())
	step := mesh.NewInstallTailscaleStep(mesh.Options{SSHDial: panicDialer})
	mt := &mesh.MeshTarget{Machine: manifestdata.Machine{Name: "gateway-host"}}
	target := scaffold.Target{ID: mt.Machine.Name, Payload: mt}

	satisfied, err := step.Check(ctx, target)
	if err != nil {
		t.Fatalf("Check on un-provisioned host returned err=%v (want nil; probe would render 'check: dial ...')", err)
	}
	if satisfied {
		t.Fatalf("Check on un-provisioned host returned satisfied=true (want false; 'will execute')")
	}
}

