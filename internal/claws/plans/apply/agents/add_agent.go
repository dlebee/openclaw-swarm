package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// defaultAgentID matches openclaw's DEFAULT_AGENT_ID. The id is reserved:
// `openclaw agents add main --non-interactive` exits 1 with "main is
// reserved", and `openclaw agents list --json` always returns a phantom
// {id:"main"} even when agents.list is empty (see
// openclaw/src/commands/agents.config.ts buildAgentSummaries fallback).
// Both behaviours break the naive add-then-configure flow, so we seed
// agents.list[] directly for "main" and skip `agents add`.
const defaultAgentID = "main"

// AddAgentStep registers the agent via `openclaw agents add` if it doesn't
// exist yet. For the reserved "main" id the CLI refuses to add, so we seed
// agents.list[] via `openclaw config set` instead — downstream steps read
// the raw list via AgentConfigIndex and don't care how the entry got there.
type AddAgentStep struct {
	dial SSHDialFunc
}

func NewAddAgentStep(opts Options) *AddAgentStep {
	return &AddAgentStep{dial: opts.SSHDial}
}

func (*AddAgentStep) Name() string { return "add-agent" }

func (s *AddAgentStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*AgentTarget)
	return ok, nil
}

func (s *AddAgentStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	// Use AgentConfigIndex (raw agents.list read) instead of FindAgent
	// (which goes through `openclaw agents list` and false-positives on
	// the phantom default for id="main" on a fresh install).
	idx, err := AgentConfigIndex(client, at.Spec.ID)
	if err != nil {
		return false, fmt.Errorf("read agents.list on %s: %w", m.Name, err)
	}
	return idx >= 0, nil
}

func (s *AddAgentStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("add-agent: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	if at.Spec.ID == defaultAgentID {
		return s.seedDefaultAgent(client, at.Spec.Workspace)
	}

	script := common.OpenclawCLIPreamble() + fmt.Sprintf(
		`openclaw agents add %q --workspace %q --model %q --non-interactive 2>&1`,
		at.Spec.ID, at.Spec.Workspace, at.Spec.Model.Primary)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("add-agent: openclaw agents add failed: %w\n%s", err, out)
	}
	return nil
}

// seedDefaultAgent writes agents.list = [{id:"main", workspace:"..."}] so
// subsequent steps (ensure-model, configure-workspace, configure-tools,
// configure-bindings) see a real entry to operate on. Workspace is resolved
// against the remote $HOME here because `agents add` (which we can't use
// for id="main") is the normal place where ~ expansion happens. Model is
// left to ensure-model.
func (s *AddAgentStep) seedDefaultAgent(client *xssh.Client, workspace string) error {
	resolved, err := resolveRemoteWorkspace(client, workspace)
	if err != nil {
		return fmt.Errorf("add-agent: resolve workspace for %q: %w", defaultAgentID, err)
	}
	entry := map[string]any{"id": defaultAgentID}
	if resolved != "" {
		entry["workspace"] = resolved
	}
	batch := []map[string]any{{"path": "agents.list", "value": []map[string]any{entry}}}
	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("add-agent: marshal seed for %q: %w", defaultAgentID, err)
	}
	script := fmt.Sprintf(`set -euo pipefail
%sopenclaw config set --batch-json '%s'
`, common.OpenclawCLIPreamble(), string(batchJSON))
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("add-agent: seed agents.list for %q failed: %w\n%s", defaultAgentID, err, out)
	}
	return nil
}

// resolveRemoteWorkspace expands a leading `~/` against the remote $HOME.
// Matches what `openclaw agents add --workspace` does server-side.
func resolveRemoteWorkspace(client *xssh.Client, workspace string) (string, error) {
	if workspace == "" {
		return "", nil
	}
	if !strings.HasPrefix(workspace, "~/") && workspace != "~" {
		return workspace, nil
	}
	home, err := gwService.ResolveHome(client)
	if err != nil {
		return "", err
	}
	if workspace == "~" {
		return home, nil
	}
	return home + strings.TrimPrefix(workspace, "~"), nil
}

func (s *AddAgentStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("add-agent verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	idx, err := AgentConfigIndex(client, at.Spec.ID)
	if err != nil {
		return fmt.Errorf("add-agent verify: %w", err)
	}
	if idx < 0 {
		return fmt.Errorf("add-agent verify: agent %q not found in agents.list after add", at.Spec.ID)
	}
	return nil
}
