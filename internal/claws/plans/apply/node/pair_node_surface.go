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
// needs surface approval after device pairing.
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

// approveNodeSurface approves the node's pending surface request on OpenClaw
// >= NodeSurfaceApprovalRequiredSince using the gateway's own auth token —
// the same approach ApproveDevice uses for device pairing. No-op on older
// releases.
func approveNodeSurface(ctx context.Context, gwClient *xssh.Client, nt *NodeTarget) error {
	ver, err := probeOpenclawVersion(gwClient)
	if err != nil {
		return fmt.Errorf("pair-node: probe version: %w", err)
	}
	if !nodeSurfaceRequired(ver) {
		return nil
	}

	// The node must reconnect after device pairing before the gateway records
	// a pending surface request. Poll until the request appears, approve it,
	// then poll until effective commands include system.run.
	const maxAttempts = 20
	for i := 0; i < maxAttempts; i++ {
		// Already satisfied (e.g. idempotent re-run).
		commands, err := readPairedNodeCommands(gwClient, nt.Spec.Name)
		if err == nil && hasAllSurfaceCommands(commands, surfaceCommandsRequired) {
			return nil
		}

		// Pending request landed — approve it via CLI with gateway token.
		reqID, err := readPendingNodeRequestID(gwClient, nt.Spec.Name)
		if err != nil {
			return fmt.Errorf("pair-node: read pending node request: %w", err)
		}
		if reqID != "" {
			if err := approveNodeSurfaceRequest(gwClient, reqID); err != nil {
				return fmt.Errorf("pair-node: approve node surface: %w", err)
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

// readPendingNodeRequestID runs `openclaw nodes list --json` on the gateway
// and returns the requestId of the pending surface request for the given
// displayName, or "" if none exists yet.
func readPendingNodeRequestID(client *xssh.Client, displayName string) (string, error) {
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw nodes list --json 2>/dev/null`)
	if err != nil {
		return "", fmt.Errorf("openclaw nodes list --json: %w", err)
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return "", nil
	}
	type pendingEntry struct {
		RequestID   string `json:"requestId"`
		DisplayName string `json:"displayName"`
	}
	var wrap struct {
		Pending []pendingEntry `json:"pending"`
	}
	if err := json.Unmarshal([]byte(body), &wrap); err != nil {
		return "", fmt.Errorf("parse openclaw nodes list --json: %w", err)
	}
	for _, p := range wrap.Pending {
		if p.DisplayName == displayName {
			return p.RequestID, nil
		}
	}
	return "", nil
}

// approveNodeSurfaceRequest runs `openclaw nodes approve <requestId>` on the
// gateway using the gateway's own auth token — the same token-bypass approach
// used by ApproveDevice for device pairing.
func approveNodeSurfaceRequest(client *xssh.Client, requestID string) error {
	tokenOut, _ := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw config get gateway.token 2>/dev/null`)
	token := strings.TrimSpace(tokenOut)

	var script string
	if token != "" && !strings.HasPrefix(token, "Config") && token != "__missing__" {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw nodes approve %q --token %q`, requestID, token)
	} else {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw nodes approve %q`, requestID)
	}
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("openclaw nodes approve %s: %w\n%s", requestID, err, out)
	}
	return nil
}

// readPairedNodeCommands runs `openclaw nodes list --json` and returns the
// effective commands for the paired node with the given displayName.
// Returns (nil, nil) when no such paired node is found yet.
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
		DisplayName string   `json:"displayName"`
		Commands    []string `json:"commands"`
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
