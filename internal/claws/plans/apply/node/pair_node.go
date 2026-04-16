package node

import (
	"context"
	"fmt"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// PairNodeStep approves the node's pending device entry on the gateway.
// This step SSHes into the gateway host to run `openclaw devices approve`.
type PairNodeStep struct {
	dial SSHDialFunc
}

func NewPairNodeStep(opts Options) *PairNodeStep {
	return &PairNodeStep{dial: opts.SSHDial}
}

func (*PairNodeStep) Name() string { return "pair-node" }

func (s *PairNodeStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*NodeTarget)
	return ok, nil
}

func (s *PairNodeStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	gwMach := nt.GWMach
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(gwMach), common.MachineSSHPort(gwMach), common.MachineSSHUser(gwMach))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	dl, err := gwService.ListDevices(client)
	if err != nil {
		return false, nil
	}
	return isNodePaired(dl, nt.Spec.Name), nil
}

func isNodePaired(dl *gwService.DeviceList, displayName string) bool {
	for _, d := range dl.Paired {
		if d.DisplayName == displayName && d.ClientMode == "node" {
			return true
		}
	}
	return false
}

func (s *PairNodeStep) Execute(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	gwMach := nt.GWMach
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.MachineHost(gwMach), common.MachineSSHPort(gwMach), common.MachineSSHUser(gwMach))
	if err != nil {
		return fmt.Errorf("pair-node: dial gateway: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// Poll for the node's pending device entry and approve it.
	const maxAttempts = 15
	for i := 0; i < maxAttempts; i++ {
		dl, err := gwService.ListDevices(client)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		if isNodePaired(dl, nt.Spec.Name) {
			return nil
		}

		for _, p := range dl.Pending {
			if p.DisplayName == nt.Spec.Name && p.Role == "node" {
				if err := gwService.ApproveDevice(client, p.RequestID); err != nil {
					return fmt.Errorf("pair-node: approve %s: %w", nt.Spec.Name, err)
				}
				return nil
			}
		}

		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("pair-node: node %q did not appear as pending device after %d attempts", nt.Spec.Name, maxAttempts)
}

func (s *PairNodeStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	gwMach := nt.GWMach
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(gwMach), common.MachineSSHPort(gwMach), common.MachineSSHUser(gwMach))
	if err != nil {
		return fmt.Errorf("pair-node verify: dial gateway: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	dl, err := gwService.ListDevices(client)
	if err != nil {
		return fmt.Errorf("pair-node verify: list devices: %w", err)
	}
	if !isNodePaired(dl, nt.Spec.Name) {
		return fmt.Errorf("pair-node verify: node %q not found in paired devices", nt.Spec.Name)
	}
	return nil
}
