package provisioning

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// CreateMachineStep provisions a Linode instance for a machine target.
type CreateMachineStep struct {
	provider  hosting.Provider
	prefix    string
	sshPubKey string
}

// NewCreateMachineStep builds the scaffold step from options.
func NewCreateMachineStep(opts Options) *CreateMachineStep {
	return &CreateMachineStep{
		provider:  opts.Provider,
		prefix:    opts.Prefix,
		sshPubKey: strings.TrimSpace(opts.SSHPubKey),
	}
}

// Name implements scaffold.Step.
func (*CreateMachineStep) Name() string { return "create-machine" }

// Applicable implements scaffold.Step — runs for any machine backed by a
// hosting.Provider (linode, multipass, …). SSH-typed machines are assumed
// pre-provisioned and skip this step entirely.
func (a *CreateMachineStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	_ = ctx
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	return manifestdata.IsHostedMachineType(mt.Spec.Type), nil
}

// Check implements scaffold.Step — list instances tagged claws/<prefix>, then match
// uniquely by Linode label (prefix-machineName).
func (a *CreateMachineStep) Check(ctx context.Context, t scaffold.Target) (satisfied bool, err error) {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, fmt.Errorf("create-machine: expected *MachineTarget payload for target %q", t.ID)
	}
	if a.provider == nil {
		return false, fmt.Errorf("create-machine: hosting provider is required for %q", t.ID)
	}
	prefixTag := clawsPrefixTag(a.prefix)
	wantLabel := machineLabel(a.prefix, mt.Spec.Name)
	instances, err := a.provider.ListByTag(ctx, prefixTag)
	if err != nil {
		return false, err
	}
	var matches []hosting.Instance
	for i := range instances {
		if instances[i].Label == wantLabel {
			matches = append(matches, instances[i])
		}
	}
	switch len(matches) {
	case 0:
		scaffold.RecordPlanMachineExists(ctx, t.ID, false)
		return false, nil
	case 1:
		inst := matches[0]
		mt.Instance = &inst
		scaffold.RecordPlanMachineExists(ctx, t.ID, true)
		// Cache the resolved IP so downstream phases (mesh, gateway, node,
		// channels, agents, automations) can dial by public IPv4 without
		// each target struct carrying a pointer back to this MachineTarget.
		scaffold.RecordPlanMachineHost(ctx, mt.Spec.Name, inst.PublicIPv4)
		return true, nil
	default:
		return false, fmt.Errorf("create-machine: %d instances with label %q under tag %q (want at most 1)",
			len(matches), wantLabel, prefixTag)
	}
}

// Execute implements scaffold.Step.
func (a *CreateMachineStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("create-machine: expected *MachineTarget payload for target %q", t.ID)
	}
	if a.provider == nil {
		return fmt.Errorf("create-machine: hosting provider is required for %q", t.ID)
	}
	if a.sshPubKey == "" {
		return fmt.Errorf("create-machine: SSH public key is empty for %q", t.ID)
	}
	rootPass, err := randomRootPassword()
	if err != nil {
		return err
	}
	spec := mt.Spec
	// Opts carry BOTH the Linode and Multipass field sets; providers ignore
	// the ones that don't apply to them. This keeps create-machine fully
	// provider-agnostic at the cost of a few unused struct fields per call.
	opts := hosting.CreateInstanceOpts{
		Label:      machineLabel(a.prefix, spec.Name),
		Image:      spec.Image,
		Tags:       []string{clawsPrefixTag(a.prefix), machineTag(a.prefix, spec.Name)},
		PublicKeys: []string{a.sshPubKey},
		// BootstrapUser tells the provider where PublicKeys need to land
		// in the fresh image (see docs on hosting.CreateInstanceOpts).
		// Empty manifest value resolves to "root" inside the provider.
		BootstrapUser: strings.TrimSpace(spec.BootstrapUser),
		// Hostname is the manifest's short machine name. Multipass sets
		// it via cloud-init so peers can dial `<name>.local` through
		// Avahi without a dynamic IP lookup; Linode ignores the field.
		Hostname: strings.TrimSpace(spec.Name),
		// Linode-specific.
		Region:   spec.Region,
		SKU:      spec.SKU,
		RootPass: rootPass,
		// Multipass-specific. Zero values are passed through and the
		// provider substitutes its own defaults if appropriate.
		CPUs:   spec.CPUs,
		Memory: spec.Memory,
		Disk:   spec.Disk,
	}
	inst, err := a.provider.CreateInstance(ctx, opts)
	if err != nil {
		return err
	}
	inst, err = a.provider.WaitRunning(ctx, inst.ResourceID)
	if err != nil {
		return err
	}
	mt.Instance = inst
	// Cache the resolved IP so downstream phases (mesh, gateway, node, etc.)
	// can dial the freshly provisioned machine by PublicIPv4. See the matching
	// RecordPlanMachineHost call in Check for the existing-instance path.
	scaffold.RecordPlanMachineHost(ctx, mt.Spec.Name, inst.PublicIPv4)
	return nil
}

// Verify implements scaffold.Step.
func (*CreateMachineStep) Verify(ctx context.Context, t scaffold.Target) error {
	_ = ctx
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("create-machine verify: expected *MachineTarget for %q", t.ID)
	}
	if mt.Instance == nil {
		return fmt.Errorf("create-machine verify: no instance on target %q", t.ID)
	}
	if mt.Instance.Status != "running" {
		return fmt.Errorf("create-machine verify: instance status %q, want running", mt.Instance.Status)
	}
	if mt.Instance.PublicIPv4 == "" {
		return fmt.Errorf("create-machine verify: empty public IPv4 for %q", t.ID)
	}
	return nil
}

func clawsPrefixTag(prefix string) string {
	return fmt.Sprintf("claws/%s", strings.TrimSpace(prefix))
}

// ClawsPrefixTag returns the Linode tag shared by all instances for a manifest prefix (claws/<prefix>).
func ClawsPrefixTag(prefix string) string {
	return clawsPrefixTag(prefix)
}

func machineTag(prefix, machineName string) string {
	return fmt.Sprintf("claws/%s/%s", prefix, machineName)
}

func machineLabel(prefix, machineName string) string {
	l := prefix + "-" + machineName
	if len(l) <= 64 {
		return l
	}
	return l[:64]
}

func randomRootPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random root password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
