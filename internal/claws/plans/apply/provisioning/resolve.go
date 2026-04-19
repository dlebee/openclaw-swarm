package provisioning

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ResolveHostedInstances populates MachineTarget.Instance for every machine
// that's backed by a hosting.Provider (linode, multipass, …) by listing
// instances under the manifest's prefix tag and matching by label. Targets
// whose Instance is already set, or whose machine type is SSH (pre-
// provisioned), are left unchanged.
//
// This is used by commands that operate outside the provisioning phase
// (e.g. `claws automations apply`, `claws destroy` cross-references) but
// still need the dynamic PublicIPv4 to reach the machine.
//
// The provider parameter is a single Provider — the caller is expected to
// have constructed the one that matches the manifest's non-SSH machine type.
// Manifests that mix multiple hosted types in one run are not supported.
func ResolveHostedInstances(ctx context.Context, provider hosting.Provider, prefix string, targets []scaffold.Target) error {
	if provider == nil {
		return nil
	}
	prefixTag := clawsPrefixTag(prefix)
	var instances []hosting.Instance
	var loaded bool
	for _, t := range targets {
		mt, ok := t.Payload.(*MachineTarget)
		if !ok || mt == nil {
			continue
		}
		if !manifestdata.IsHostedMachineType(mt.Spec.Type) {
			continue
		}
		if mt.Instance != nil {
			continue
		}
		if !loaded {
			var err error
			instances, err = provider.ListByTag(ctx, prefixTag)
			if err != nil {
				return fmt.Errorf("resolve hosted instances: %w", err)
			}
			loaded = true
		}
		want := machineLabel(prefix, mt.Spec.Name)
		for i := range instances {
			if instances[i].Label == want {
				inst := instances[i]
				mt.Instance = &inst
				// Mirror create-machine: seed the plan cache so downstream
				// phases resolve the PublicIPv4 through the usual
				// common.ResolveMachineHost path.
				scaffold.RecordPlanMachineHost(ctx, mt.Spec.Name, inst.PublicIPv4)
				break
			}
		}
	}
	return nil
}

// ResolveLinodeInstances is a deprecated alias preserved so external callers
// (and older generated docs) keep compiling. New code should call
// ResolveHostedInstances.
//
// Deprecated: use ResolveHostedInstances.
func ResolveLinodeInstances(ctx context.Context, provider hosting.Provider, prefix string, targets []scaffold.Target) error {
	return ResolveHostedInstances(ctx, provider, prefix, targets)
}
