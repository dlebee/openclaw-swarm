package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// PairNodeStep approves the node's pending device entry on the gateway
// and, on OpenClaw >= 2026.5.18, promotes the node's command surface so
// agents can use system.run over the node websocket. Older OpenClaw
// releases only need device pairing; surface approval is a no-op there.
type PairNodeStep struct {
	dial         SSHDialFunc
	reader       common.ConfigReader
	hostResolver common.HostResolverFn
}

func NewPairNodeStep(opts Options) *PairNodeStep {
	r := opts.ConfigReader
	if r == nil {
		r = common.DefaultConfigReader(opts.SSHDial)
	}
	return &PairNodeStep{dial: opts.SSHDial, reader: r, hostResolver: opts.HostResolver}
}

func (*PairNodeStep) Name() string { return "pair-node" }

func (s *PairNodeStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*NodeTarget)
	return ok, nil
}

func (s *PairNodeStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	gwMach := nt.GWMach
	host, ok := common.HostKnown(ctx, gwMach, s.hostResolver)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
	if err != nil {
		return false, fmt.Errorf("dial gateway %s: %w", gwMach.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	cfgHost := common.MachineConfigHost(gwMach, host)
	dl, err := s.reader.DeviceList(ctx, client, cfgHost)
	if err != nil {
		return false, fmt.Errorf("list devices on %s: %w", gwMach.Name, err)
	}
	if !isNodePaired(dl, nt.Spec.Name) {
		return false, nil
	}
	okSurface, err := nodeSurfaceSatisfied(client, nt.Spec.Name)
	if err != nil {
		return false, fmt.Errorf("pair-node check: %w", err)
	}
	return okSurface, nil
}

func isNodePaired(dl *common.DeviceList, displayName string) bool {
	if dl == nil {
		return false
	}
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
	gwHost := common.ResolveMachineHost(ctx, gwMach)
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, gwHost, common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
	if err != nil {
		return fmt.Errorf("pair-node: dial gateway: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)
	cfgHost := common.MachineConfigHost(gwMach, gwHost)

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
		dl, err := s.reader.DeviceList(ctx, client, cfgHost)
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
		// Collect diagnostics to understand why the node didn't appear.
		diag := collectNodePairingDiagnostics(ctx, s.dial, nt, client)
		if sawPending && lastApproveErr != nil {
			return fmt.Errorf("pair-node: node %q remained unpaired after %d approve attempts; last approve error: %w\n%s",
				nt.Spec.Name, maxAttempts, lastApproveErr, diag)
		}
		return fmt.Errorf("pair-node: node %q did not appear as pending device after %d attempts\n%s", nt.Spec.Name, maxAttempts, diag)
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

	// OpenClaw >= 2026.5.18: approve the pending node surface (system.run)
	// after the node reconnects. No-op on older releases.
	return approveNodeSurface(ctx, s.dial, client, nt)
}

// collectNodePairingDiagnostics gathers info to debug why a node didn't
// appear as a pending device on the gateway.
func collectNodePairingDiagnostics(ctx context.Context, dial SSHDialFunc, nt *NodeTarget, gwClient *xssh.Client) string {
	var diag []string

	// 1. Check node daemon status on the node machine
	nodeMach := nt.Machine
	nodeHost := common.ResolveMachineHost(ctx, nodeMach)
	nodeClient, nodeKey, err := common.BorrowSSH(ctx, dial, nodeHost, common.MachineSSHPort(nodeMach), common.MachineAgentUser(nodeMach))
	if err != nil {
		diag = append(diag, fmt.Sprintf("[node-diag] failed to SSH to node: %v", err))
	} else {
		defer common.ReturnSSH(ctx, nodeKey, nodeClient)

		// Node daemon status
		status, _ := bash.RunOutput(nodeClient, `systemctl --user is-active openclaw-node 2>&1 || true`)
		diag = append(diag, fmt.Sprintf("[node-diag] node daemon status: %s", strings.TrimSpace(status)))

		// Node daemon status (detailed) + journal
		statusOut, _ := bash.RunOutput(nodeClient, `export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user status openclaw-node 2>&1 || true`)
		diag = append(diag, fmt.Sprintf("[node-diag] systemctl status:\n%s", strings.TrimSpace(statusOut)))

		logs, _ := bash.RunOutput(nodeClient, `export XDG_RUNTIME_DIR=/run/user/$(id -u); journalctl --user -u openclaw-node -n 30 --no-pager 2>&1 || true`)
		diag = append(diag, fmt.Sprintf("[node-diag] node daemon logs:\n%s", strings.TrimSpace(logs)))

		// Check if node binary exists and is executable
		nodeBin, _ := bash.RunOutput(nodeClient, `which openclaw 2>&1 && openclaw --version 2>&1 || echo "not found"`)
		diag = append(diag, fmt.Sprintf("[node-diag] openclaw binary: %s", strings.TrimSpace(nodeBin)))

		// Check node config
		nodeConfig, _ := bash.RunOutput(nodeClient, `cat ~/.openclaw/openclaw.json 2>&1 | head -20 || echo "no config"`)
		diag = append(diag, fmt.Sprintf("[node-diag] node config: %s", strings.TrimSpace(nodeConfig)))

		// Check the systemd unit file and env drop-in
		nodeUnitContent, _ := bash.RunOutput(nodeClient, `cat ~/.config/systemd/user/openclaw-node.service 2>&1 || echo "no unit"`)
		diag = append(diag, fmt.Sprintf("[node-diag] systemd unit:\n%s", strings.TrimSpace(nodeUnitContent)))

		// Check openclaw data dir
		dataDir, _ := bash.RunOutput(nodeClient, `ls -la ~/.openclaw/ 2>&1 || echo "no .openclaw dir"`)
		diag = append(diag, fmt.Sprintf("[node-diag] .openclaw dir: %s", strings.TrimSpace(dataDir)))

		// Can node reach gateway via headscale?
		gwHost := nt.GatewayInternalHost(ctx, dial)
		if gwHost != "" {
			ping, _ := bash.RunOutput(nodeClient, fmt.Sprintf(`ping -c 1 -W 2 %s 2>&1 || echo "ping failed"`, gwHost))
			diag = append(diag, fmt.Sprintf("[node-diag] ping gateway (%s): %s", gwHost, strings.TrimSpace(ping)))
		}

		// Tailscale status on node
		tsStatus, _ := bash.RunOutput(nodeClient, `tailscale status 2>&1 | head -5 || true`)
		diag = append(diag, fmt.Sprintf("[node-diag] tailscale status: %s", strings.TrimSpace(tsStatus)))
	}

	// 2. Check gateway's view of pending/paired devices (use token auth)
	token := common.ReadGatewayAuthToken(gwClient)
	diag = append(diag, fmt.Sprintf("[gw-diag] token length: %d", len(token)))
	
	var devScript string
	if token != "" {
		devScript = fmt.Sprintf(`openclaw devices list --json --token %q --url ws://127.0.0.1:18789 2>&1 | head -50 || true`, token)
		diag = append(diag, "[gw-diag] using token auth for devices list")
	} else {
		devScript = `openclaw devices list --json 2>&1 | head -50 || true`
		diag = append(diag, "[gw-diag] NO TOKEN - using default auth for devices list")
	}
	devList, _ := bash.RunOutput(gwClient, common.OpenclawCLIPreamble()+devScript)
	diag = append(diag, fmt.Sprintf("[gw-diag] devices list: %s", strings.TrimSpace(devList)))

	// Gateway daemon logs
	gwLogs, _ := bash.RunOutput(gwClient, `export XDG_RUNTIME_DIR=/run/user/$(id -u); journalctl --user -u openclaw-gateway -n 15 --no-pager 2>&1 || true`)
	diag = append(diag, fmt.Sprintf("[gw-diag] gateway logs:\n%s", strings.TrimSpace(gwLogs)))

	return strings.Join(diag, "\n")
}

func (s *PairNodeStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	gwMach := nt.GWMach
	gwHost := common.ResolveMachineHost(ctx, gwMach)
	client, key, err := common.BorrowSSH(ctx, s.dial, gwHost, common.MachineSSHPort(gwMach), common.MachineAgentUser(gwMach))
	if err != nil {
		return fmt.Errorf("pair-node verify: dial gateway: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	cfgHost := common.MachineConfigHost(gwMach, gwHost)
	dl, err := s.reader.DeviceList(ctx, client, cfgHost)
	if err != nil {
		return fmt.Errorf("pair-node verify: list devices: %w", err)
	}
	if !isNodePaired(dl, nt.Spec.Name) {
		return fmt.Errorf("pair-node verify: node %q not found in paired devices", nt.Spec.Name)
	}
	okSurface, err := nodeSurfaceSatisfied(client, nt.Spec.Name)
	if err != nil {
		return fmt.Errorf("pair-node verify: %w", err)
	}
	if !okSurface {
		commands, _ := readPairedNodeCommands(client, nt.Spec.Name)
		return fmt.Errorf("pair-node verify: node %q effective commands %v missing required surface %v",
			nt.Spec.Name, commands, surfaceCommandsRequired)
	}
	return nil
}
