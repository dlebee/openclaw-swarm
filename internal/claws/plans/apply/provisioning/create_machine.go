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

// CreateMachineAction provisions a Linode instance for a machine target.
type CreateMachineAction struct {
	provider  hosting.Provider
	prefix    string
	sshPubKey string
}

// NewCreateMachineAction builds the scaffold action from options.
func NewCreateMachineAction(opts Options) *CreateMachineAction {
	return &CreateMachineAction{
		provider:  opts.Provider,
		prefix:    opts.Prefix,
		sshPubKey: strings.TrimSpace(opts.SSHPubKey),
	}
}

// Name implements scaffold.Action.
func (*CreateMachineAction) Name() string { return "create-machine" }

// Applicable implements scaffold.Action — only Linode machines are provisioned here.
func (a *CreateMachineAction) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	_ = ctx
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	return mt.Spec.Type == manifestdata.MachineTypeLinode, nil
}

// Check implements scaffold.Action — detect already-tagged instances.
func (a *CreateMachineAction) Check(ctx context.Context, t scaffold.Target) (blocked bool, err error) {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, fmt.Errorf("create-machine: expected *MachineTarget payload for target %q", t.ID)
	}
	if a.provider == nil {
		return false, fmt.Errorf("create-machine: hosting provider is required for %q", t.ID)
	}
	tag := machineTag(a.prefix, mt.Spec.Name)
	instances, err := a.provider.ListByTag(ctx, tag)
	if err != nil {
		return false, err
	}
	if len(instances) == 0 {
		return false, nil
	}
	inst := instances[0]
	mt.Instance = &inst
	return true, nil
}

// Execute implements scaffold.Action.
func (a *CreateMachineAction) Execute(ctx context.Context, t scaffold.Target) error {
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
	opts := hosting.CreateInstanceOpts{
		Label:      machineLabel(a.prefix, spec.Name),
		Region:     spec.Region,
		SKU:        spec.SKU,
		Image:      spec.Image,
		Tags:       []string{machineTag(a.prefix, spec.Name)},
		PublicKeys: []string{a.sshPubKey},
		RootPass:   rootPass,
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
	return nil
}

// Verify implements scaffold.Action.
func (*CreateMachineAction) Verify(ctx context.Context, t scaffold.Target) error {
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

func machineTag(prefix, machineName string) string {
	return fmt.Sprintf("claws:%s:%s", prefix, machineName)
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
	// Linode accepts long passwords; hex is simple and URL-safe enough for API.
	return hex.EncodeToString(b), nil
}
