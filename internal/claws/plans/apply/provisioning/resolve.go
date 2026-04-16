package provisioning

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ResolveLinodeInstances populates MachineTarget.Instance for Linode-typed
// targets by listing instances under the manifest's prefix tag and matching by
// label. Targets whose Instance is already set or whose machine type is not
// Linode are left unchanged.
//
// This is useful for commands that operate outside the provisioning phase
// (e.g. `claws automations apply`) but still need the dynamic PublicIPv4 to
// reach Linode machines.
func ResolveLinodeInstances(ctx context.Context, provider hosting.Provider, prefix string, targets []scaffold.Target) error {
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
		if mt.Spec.Type != manifestdata.MachineTypeLinode {
			continue
		}
		if mt.Instance != nil {
			continue
		}
		if !loaded {
			var err error
			instances, err = provider.ListByTag(ctx, prefixTag)
			if err != nil {
				return fmt.Errorf("resolve linode instances: %w", err)
			}
			loaded = true
		}
		want := machineLabel(prefix, mt.Spec.Name)
		for i := range instances {
			if instances[i].Label == want {
				inst := instances[i]
				mt.Instance = &inst
				break
			}
		}
	}
	return nil
}
