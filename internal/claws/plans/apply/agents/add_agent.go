package agents

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// AddAgentStep registers the agent via `openclaw agents add` if it doesn't
// exist yet. Not applicable when the agent is already in the list.
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
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	agent, err := FindAgent(client, at.Spec.ID)
	if err != nil {
		return false, nil
	}
	return agent != nil, nil
}

func (s *AddAgentStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("add-agent: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	script := fmt.Sprintf(
		`openclaw agents add %q --workspace %q --model %q --non-interactive 2>&1`,
		at.Spec.ID, at.Spec.Workspace, at.Spec.Model.Primary)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("add-agent: openclaw agents add failed: %w\n%s", err, out)
	}
	return nil
}

func (s *AddAgentStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("add-agent verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	agent, err := FindAgent(client, at.Spec.ID)
	if err != nil {
		return fmt.Errorf("add-agent verify: %w", err)
	}
	if agent == nil {
		return fmt.Errorf("add-agent verify: agent %q not found after add", at.Spec.ID)
	}
	return nil
}
