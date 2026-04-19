package node

import (
	"context"
	"fmt"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
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
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, gwMach), common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
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
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, gwMach), common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
	if err != nil {
		return fmt.Errorf("pair-node: dial gateway: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	// Poll for the node's pending device entry and approve it.
	//
	// The openclaw CLI on a CPU-starved host (e.g. Linode
	// g6-standard-1, 1 vCPU) routinely trips its hard-coded 10 s
	// gateway handshake timer while Node.js is still loading the
	// CLI bundle, yielding a spurious "gateway timeout after
	// 10000ms" on approve even though the daemon received and
	// processed the request. We therefore treat approve failures
	// as soft — the next ListDevices iteration (which falls back
	// to reading the daemon's on-disk paired.json when the CLI
	// also times out) will observe the device as paired and break
	// the loop via isNodePaired. approve is idempotent on
	// requestId, so re-sending is safe when the daemon genuinely
	// missed the first one.
	const maxAttempts = 15
	approved := false
	sawPending := false
	var lastApproveErr error
	for i := 0; i < maxAttempts; i++ {
		dl, err := gwService.ListDevices(client)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		if isNodePaired(dl, nt.Spec.Name) {
			approved = true
			break
		}

		for _, p := range dl.Pending {
			if p.DisplayName == nt.Spec.Name && p.Role == "node" {
				sawPending = true
				if err := gwService.ApproveDevice(client, p.RequestID); err != nil {
					lastApproveErr = err
					break
				}
				approved = true
				break
			}
		}
		if approved {
			break
		}

		time.Sleep(2 * time.Second)
	}
	if !approved {
		if sawPending && lastApproveErr != nil {
			return fmt.Errorf("pair-node: node %q remained unpaired after %d approve attempts; last approve error: %w",
				nt.Spec.Name, maxAttempts, lastApproveErr)
		}
		return fmt.Errorf("pair-node: node %q did not appear as pending device after %d attempts", nt.Spec.Name, maxAttempts)
	}

	// The node daemon may have exited after its initial connection was
	// rejected (pairing required). Restart it so it reconnects now that
	// the device is approved.
	m := nt.Machine
	nodeClient, nodeKey, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("pair-node: dial node for restart: %w", err)
	}
	defer common.ReturnSSH(ctx, nodeKey, nodeClient)

	if err := systemd.Restart(nodeClient, nodeUnit, true); err != nil {
		return fmt.Errorf("pair-node: restart node daemon: %w", err)
	}
	return nil
}

func (s *PairNodeStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	gwMach := nt.GWMach
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, gwMach), common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
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
