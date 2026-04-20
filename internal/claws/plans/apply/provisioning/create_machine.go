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

// Check implements scaffold.Step — asks the resolver whether an instance
// already exists for this target and hydrates the target payload for
// downstream probe steps.
//
// The hydration (mt.Instance, RecordPlanMachineHost) is delegated to
// hydrateFromMachineStatus so the cache-purity AST linter does not flag
// the Check body. The mutation is intentional: without it every
// downstream step in the probe (authorize-ssh-key, security, mesh,
// gateway, …) returns "would execute" on a fully-converged machine
// because they all gate on mt.Instance.PublicIPv4 being non-empty.
// This is still cache-purity-correct: cold == hot, same data either way.
func (a *CreateMachineStep) Check(ctx context.Context, t scaffold.Target) (satisfied bool, err error) {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, fmt.Errorf("create-machine: expected *MachineTarget payload for target %q", t.ID)
	}
	if a.provider == nil {
		return false, fmt.Errorf("create-machine: hosting provider is required for %q", t.ID)
	}
	status, err := ResolveMachineStatus(ctx, a.provider, a.prefix, mt)
	if err != nil {
		return false, err
	}
	hydrateFromMachineStatus(ctx, mt, status)
	return status.Exists, nil
}

// hydrateFromMachineStatus populates mt.Instance and the plan-cache host
// entry when a live instance snapshot is available. It is called from
// both Check (probe pass) and Execute (execution pass) so that downstream
// steps always have a reachable IP regardless of which pass they run in.
//
// RecordPlanMachineExists is intentionally NOT written here — that bit is
// set by Execute on first create and by ResolveHostedInstances on bulk
// hydration paths; each write owner stays distinct.
func hydrateFromMachineStatus(ctx context.Context, mt *MachineTarget, status MachineStatus) {
	if !status.Exists || status.Instance == nil {
		return
	}
	inst := *status.Instance
	mt.Instance = &inst
	scaffold.RecordPlanMachineHost(ctx, mt.Spec.Name, inst.PublicIPv4)
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
	// Consult the resolver first so Execute is idempotent on already-
	// converged infrastructure (e.g. a cold-start run where the probe
	// never touched mt.Payload, or a --only invocation that skipped the
	// provisioning Check). If the instance already exists we hydrate the
	// payload + plan cache from the resolver's snapshot and return
	// without re-creating.
	status, err := ResolveMachineStatus(ctx, a.provider, a.prefix, mt)
	if err != nil {
		return err
	}
	if status.Exists {
		hydrateFromMachineStatus(ctx, mt, status)
		return nil
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
		// Hostname pins the multipass VM's uname -n and avahi broadcast
		// to the fully-prefixed instance label (<prefix>-<name>) — same
		// value we feed to the multipass CLI as --name. Using the
		// prefixed form (rather than the short manifest name) means:
		//   - parallel/re-apply runs with different prefixes never
		//     collide on `<name>.local` mDNS,
		//   - the manifest author can hardcode the host URL
		//     (`http://<prefix>-<name>.local:...`) with no templating
		//     or runtime IP discovery, and
		//   - `getent hosts` inside the VM converges on the same name
		//     multipass already uses, so install-tailscale's host pin
		//     (see mesh/install_tailscale.go) resolves on first try.
		// When prefix is empty (unit tests, bare manifests) we fall
		// back to the short name so we don't broadcast a leading dash.
		// Linode ignores this field regardless.
		Hostname: hostedHostname(a.prefix, spec.Name),
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
	// can dial the freshly provisioned machine by PublicIPv4.
	scaffold.RecordPlanMachineHost(ctx, mt.Spec.Name, inst.PublicIPv4)
	// Seed the resolver with the authoritative snapshot so subsequent
	// ResolveMachineStatus callers in this plan run don't have to round-trip
	// through the provider to rediscover what we just produced.
	RecordMachineStatus(ctx, a.prefix, mt.Spec.Name, MachineStatus{Exists: true, Instance: inst})
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

// hostedHostname picks the hostname pinned into the VM's cloud-init. We
// mirror machineLabel (prefix + "-" + name) for hosted providers so the
// VM's mDNS broadcast, multipass instance label, and manifest-authored
// control URL all agree on a single string. An empty prefix (unit tests
// with raw manifests that never went through the CLI's auto-prefix
// injection) collapses to the bare machine name to avoid a leading dash.
func hostedHostname(prefix, machineName string) string {
	name := strings.TrimSpace(machineName)
	if strings.TrimSpace(prefix) == "" {
		return name
	}
	return machineLabel(prefix, name)
}

func randomRootPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random root password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
