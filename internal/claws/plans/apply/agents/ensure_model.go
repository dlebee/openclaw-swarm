package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnsureModelStep ensures the agent's model config (primary + fallbacks)
// matches the manifest. Drift-repairs via `openclaw config set --batch-json`.
type EnsureModelStep struct {
	dial SSHDialFunc
}

func NewEnsureModelStep(opts Options) *EnsureModelStep {
	return &EnsureModelStep{dial: opts.SSHDial}
}

func (*EnsureModelStep) Name() string { return "ensure-model" }

func (s *EnsureModelStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*AgentTarget)
	return ok, nil
}

func (s *EnsureModelStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	agent, err := FindAgent(client, at.Spec.ID)
	if err != nil || agent == nil {
		return false, nil
	}
	return agent.Model == at.Spec.Model.Primary, nil
}

func (s *EnsureModelStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("ensure-model: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	idx, err := AgentConfigIndex(client, at.Spec.ID)
	if err != nil || idx < 0 {
		return fmt.Errorf("ensure-model: agent %q not found in config", at.Spec.ID)
	}

	type batchEntry struct {
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}

	batch := []batchEntry{
		{Path: fmt.Sprintf("agents.list[%d].model", idx), Value: at.Spec.Model.Primary},
	}
	if len(at.Spec.Model.Fallbacks) > 0 {
		batch = append(batch, batchEntry{
			Path:  fmt.Sprintf("agents.list[%d].fallbacks", idx),
			Value: at.Spec.Model.Fallbacks,
		})
	}

	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("ensure-model: marshal: %w", err)
	}

	script := fmt.Sprintf(`set -euo pipefail
openclaw config set --batch-json '%s'
`, string(batchJSON))

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("ensure-model: config set: %w\n%s", err, out)
	}
	return nil
}

func (s *EnsureModelStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("ensure-model verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	agent, err := FindAgent(client, at.Spec.ID)
	if err != nil {
		return fmt.Errorf("ensure-model verify: %w", err)
	}
	if agent == nil {
		return fmt.Errorf("ensure-model verify: agent %q not found", at.Spec.ID)
	}
	if agent.Model != at.Spec.Model.Primary {
		return fmt.Errorf("ensure-model verify: model %q, want %q", agent.Model, at.Spec.Model.Primary)
	}
	return nil
}
