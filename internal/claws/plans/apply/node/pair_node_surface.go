package node

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/openclawver"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	xssh "golang.org/x/crypto/ssh"
)

// NodeSurfaceApprovalRequiredSince is the lowest OpenClaw release that
// requires node-surface approval after device pairing in order to
// populate a node's effective command surface (caps + commands +
// permissions). Introduced by gateway commit 6160e7a411
// "fix(gateway): hide unapproved node surfaces" (May 2026).
//
// pair-node gates on this version internally; operators always see a
// single "pair-node" step regardless of OpenClaw release.
var NodeSurfaceApprovalRequiredSince = openclawver.MustParse("2026.5.18")

// surfaceCommandsRequired lists the node command(s) pair-node uses to
// declare the surface effectively approved. system.run is the only one
// swarm tests dispatch over the node websocket (cron-agent /
// exec-host-node path).
var surfaceCommandsRequired = []string{"system.run"}

// nodeSurfaceRequired reports whether the gateway OpenClaw version
// needs the on-disk nodes/paired.json promote path after device pairing.
func nodeSurfaceRequired(ver openclawver.Version) bool {
	return ver.AtLeast(NodeSurfaceApprovalRequiredSince)
}

// nodeSurfaceSatisfied returns true when no surface approval is needed
// (older OpenClaw) or when effective commands include everything in
// surfaceCommandsRequired.
func nodeSurfaceSatisfied(client *xssh.Client, displayName string) (bool, error) {
	ver, err := probeOpenclawVersion(client)
	if err != nil {
		return false, err
	}
	if !nodeSurfaceRequired(ver) {
		return true, nil
	}
	commands, err := readPairedNodeCommands(client, displayName)
	if err != nil {
		return false, err
	}
	return hasAllSurfaceCommands(commands, surfaceCommandsRequired), nil
}

// approveNodeSurface promotes a pending node-pair request on OpenClaw
// >= NodeSurfaceApprovalRequiredSince. No-op on older releases.
func approveNodeSurface(ctx context.Context, dial SSHDialFunc, gwClient *xssh.Client, nt *NodeTarget) error {
	ver, err := probeOpenclawVersion(gwClient)
	if err != nil {
		return fmt.Errorf("pair-node: probe version: %w", err)
	}
	if !nodeSurfaceRequired(ver) {
		return nil
	}

	const maxAttempts = 20
	for i := 0; i < maxAttempts; i++ {
		commands, err := readPairedNodeCommands(gwClient, nt.Spec.Name)
		if err == nil && hasAllSurfaceCommands(commands, surfaceCommandsRequired) {
			return nil
		}
		patched, err := promotePendingNodeToPaired(gwClient, nt.Spec.Name)
		if err == nil && patched {
			if err := restartNodeDaemon(ctx, dial, nt); err != nil {
				return fmt.Errorf("pair-node: restart node after surface promote: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	commands, _ := readPairedNodeCommands(gwClient, nt.Spec.Name)
	if hasAllSurfaceCommands(commands, surfaceCommandsRequired) {
		return nil
	}
	return fmt.Errorf("pair-node: %q command surface not approved after %d attempts (got commands=%v, want %v)",
		nt.Spec.Name, maxAttempts, commands, surfaceCommandsRequired)
}

func probeOpenclawVersion(client *xssh.Client) (openclawver.Version, error) {
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw --version`)
	if err != nil {
		return openclawver.Version{}, fmt.Errorf("openclaw --version: %w", err)
	}
	return openclawver.Parse(out)
}

func promotePendingNodeToPaired(client *xssh.Client, displayName string) (bool, error) {
	script := fmt.Sprintf(`set -euo pipefail
PENDING=~/.openclaw/nodes/pending.json
PAIRED=~/.openclaw/nodes/paired.json
[ -f "$PENDING" ] || { echo "no-pending-file" >&2; exit 0; }
[ -f "$PAIRED" ]  || echo "{}" > "$PAIRED"

MATCH=$(jq -r --arg name %q '
  to_entries[]
  | select(.value.displayName == $name)
  | .key
' "$PENDING" | head -1)
if [ -z "$MATCH" ]; then
  echo "no-pending-match" >&2
  exit 0
fi

NOW=$(date +%%s%%3N)
TOKEN=$(openssl rand -base64 32 | tr -d '/+=\n' | head -c 32)

TMP=$(mktemp /tmp/paired-nodes.json.XXXXXX)
jq --arg req "$MATCH" --arg now "$NOW" --arg token "$TOKEN" --slurpfile paired "$PAIRED" '
  (.[$req]) as $r
  | ($paired[0] // {}) + {
      ($r.nodeId): {
        nodeId: $r.nodeId,
        token: $token,
        clientId: "node-host",
        clientMode: "node",
        displayName: $r.displayName,
        platform: $r.platform,
        version: $r.version,
        coreVersion: $r.coreVersion,
        uiVersion: $r.uiVersion,
        deviceFamily: $r.deviceFamily,
        modelIdentifier: $r.modelIdentifier,
        caps: $r.caps,
        commands: $r.commands,
        permissions: $r.permissions,
        remoteIp: $r.remoteIp,
        createdAtMs: ($now | tonumber),
        approvedAtMs: ($now | tonumber)
      }
    }
' "$PENDING" > "$TMP"
mv "$TMP" "$PAIRED"

TMP=$(mktemp /tmp/pending-nodes.json.XXXXXX)
jq --arg req "$MATCH" 'del(.[$req])' "$PENDING" > "$TMP"
mv "$TMP" "$PENDING"

echo "promoted" >&2

export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"
systemctl --user restart openclaw-gateway 2>/dev/null || \
  sudo systemctl restart openclaw-gateway 2>/dev/null || true
sleep 4
`, displayName)
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return false, fmt.Errorf("promote pending node: %w\n%s", err, out)
	}
	return strings.Contains(out, "promoted"), nil
}

func restartNodeDaemon(ctx context.Context, dial SSHDialFunc, nt *NodeTarget) error {
	m := nt.Machine
	host := common.ResolveMachineHost(ctx, m)
	client, key, err := common.BorrowSSH(ctx, dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("dial node %s: %w", m.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)
	script := `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"
systemctl --user restart openclaw-node 2>/dev/null || \
  sudo systemctl restart openclaw-node 2>/dev/null || true
sleep 4
`
	if _, err := bash.RunOutput(client, script); err != nil {
		return fmt.Errorf("restart openclaw-node: %w", err)
	}
	return nil
}

func readPairedNodeCommands(client *xssh.Client, displayName string) ([]string, error) {
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw nodes list --json 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("openclaw nodes list --json: %w", err)
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return nil, nil
	}
	type nodeEntry struct {
		NodeID      string   `json:"nodeId"`
		DisplayName string   `json:"displayName"`
		Commands    []string `json:"commands"`
		Connected   bool     `json:"connected"`
		Paired      bool     `json:"paired"`
	}
	if body[0] == '{' {
		var wrap struct {
			Nodes  []nodeEntry `json:"nodes"`
			Paired []nodeEntry `json:"paired"`
		}
		if err := json.Unmarshal([]byte(body), &wrap); err != nil {
			return nil, fmt.Errorf("parse openclaw nodes list --json (object form): %w", err)
		}
		for _, n := range append(wrap.Nodes, wrap.Paired...) {
			if n.DisplayName == displayName {
				return n.Commands, nil
			}
		}
		return nil, nil
	}
	var arr []nodeEntry
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		return nil, fmt.Errorf("parse openclaw nodes list --json (array form): %w", err)
	}
	for _, n := range arr {
		if n.DisplayName == displayName {
			return n.Commands, nil
		}
	}
	return nil, nil
}

func hasAllSurfaceCommands(commands, want []string) bool {
	have := make(map[string]struct{}, len(commands))
	for _, c := range commands {
		have[c] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			return false
		}
	}
	return true
}
