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

func TestResolveControlURLCheck_isPureCacheRead(t *testing.T) {
	// resolve-control-url.Check is a pure cache read — it must not
	// derive the URL from the manifest (even for strategy=custom
	// where no SSH is needed). A brand-new cold-start apply must
	// render "will execute" in the probe preview, not "satisfied",
	// because no other step has populated the cache yet.
	ctx := scaffold.EnsurePlanCache(context.Background())
	step := mesh.NewResolveControlURLStep(mesh.Options{SSHDial: panicDialer})
	gw := &manifestdata.Gateway{
		Name:      "gw",
		Reference: "gateway-host",
		// strategy=custom with a fully-specified host would
		// previously short-circuit Check to "satisfied" because the
		// URL is derivable from manifest alone. The new Check
		// ignores the manifest entirely and only reads the cache.
		Networking: &manifestdata.GatewayNetworking{
			Mode: "headscale",
			PublicHostname: &manifestdata.PublicHostnameSpec{
				Strategy: "custom",
				Host:     "gw.example.com",
			},
		},
	}
	mt := &mesh.MeshTarget{
		Machine:       manifestdata.Machine{Name: "gateway-host"},
		Gateway:       gw,
		IsGatewayHost: true,
	}
	target := scaffold.Target{ID: mt.Machine.Name, Payload: mt}

	satisfied, err := step.Check(ctx, target)
	if err != nil {
		t.Fatalf("Check returned err=%v (want nil)", err)
	}
	if satisfied {
		t.Fatalf("Check returned satisfied=true on empty cache (want false; probe UI should say 'will execute')")
	}

	// Populate cache; Check should now report satisfied without
	// dialing (panicDialer proves we never reach BorrowSSH).
	scaffold.PlanCacheSet(ctx, mesh.CacheKeyControlURL, "https://gw.example.com")
	satisfied, err = step.Check(ctx, target)
	if err != nil {
		t.Fatalf("Check with seeded cache returned err=%v", err)
	}
	if !satisfied {
		t.Fatalf("Check with seeded cache returned satisfied=false (want true)")
	}
}

func TestResolvePreauthKeyCheck_isPureCacheRead(t *testing.T) {
	// resolve-preauth-key.Check is a pure cache read — it must not
	// consult env vars or SFTP the gateway disk. A brand-new
	// cold-start apply with OPENCLAW_HEADSCALE_PREAUTH_KEY exported
	// must still render "will execute" in the probe preview, not
	// "satisfied".
	ctx := scaffold.EnsurePlanCache(context.Background())
	step := mesh.NewResolvePreauthKeyStep(mesh.Options{SSHDial: panicDialer})
	gw := &manifestdata.Gateway{
		Name:      "gw",
		Reference: "gateway-host",
		Networking: &manifestdata.GatewayNetworking{
			Mode:             "headscale",
			PreauthKeySource: "env",
			PreauthKeyEnv:    "TEST_HEADSCALE_PREAUTH_KEY_DOES_NOT_EXIST",
		},
	}
	mt := &mesh.MeshTarget{
		Machine:       manifestdata.Machine{Name: "gateway-host"},
		Gateway:       gw,
		IsGatewayHost: true,
	}
	target := scaffold.Target{ID: mt.Machine.Name, Payload: mt}

	satisfied, err := step.Check(ctx, target)
	if err != nil {
		t.Fatalf("Check returned err=%v", err)
	}
	if satisfied {
		t.Fatalf("Check returned satisfied=true on empty cache (want false)")
	}

	scaffold.PlanCacheSet(ctx, mesh.CacheKeyPreauthKey, "tskey-auth-ABCDEF")
	satisfied, err = step.Check(ctx, target)
	if err != nil {
		t.Fatalf("Check with seeded cache returned err=%v", err)
	}
	if !satisfied {
		t.Fatalf("Check with seeded cache returned satisfied=false (want true)")
	}
}
