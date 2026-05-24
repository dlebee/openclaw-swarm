package agents

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ConfigureBindingsStep ensures the agent's channel bindings match the
// manifest. Adds missing bindings and removes extras.
type ConfigureBindingsStep struct {
	dial   SSHDialFunc
	reader common.ConfigReader
}

func NewConfigureBindingsStep(opts Options) *ConfigureBindingsStep {
	return &ConfigureBindingsStep{dial: opts.SSHDial, reader: opts.ConfigReader}
}

func (*ConfigureBindingsStep) Name() string { return "configure-bindings" }

func (s *ConfigureBindingsStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	at, ok := t.Payload.(*AgentTarget)
	if !ok {
		return false, nil
	}
	return len(at.Spec.Bindings) > 0, nil
}

func (s *ConfigureBindingsStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	at := t.Payload.(*AgentTarget)
	if len(at.Spec.Bindings) == 0 {
		return true, nil
	}
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

	current, err := s.reader.AgentBindings(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
	if err != nil {
		return false, fmt.Errorf("list bindings on %s: %w", m.Name, err)
	}

	toAdd, toRemove := diffBindings(current, at.Spec.Bindings)
	return len(toAdd) == 0 && len(toRemove) == 0, nil
}

func (s *ConfigureBindingsStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	if len(at.Spec.Bindings) == 0 {
		return nil
	}
	m := at.Machine
	host := common.ResolveMachineHost(ctx, m)
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-bindings: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	current, err := s.reader.AgentBindings(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
	if err != nil {
		return fmt.Errorf("configure-bindings: list: %w", err)
	}

	toAdd, toRemove := diffBindings(current, at.Spec.Bindings)

	// Serialize config mutations per machine to avoid ConfigMutationConflictError
	for _, b := range toRemove {
		script := common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw agents unbind --agent %q --bind %q`, at.Spec.ID, formatBinding(b))
		var out string
		err := common.WithConfigMutationLock(m.Name, func() error {
			var runErr error
			out, runErr = bash.RunOutput(client, script)
			return runErr
		})
		if err != nil {
			return fmt.Errorf("configure-bindings: unbind %s: %w\n%s", formatBinding(b), err, out)
		}
	}

	for _, b := range toAdd {
		script := common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw agents bind --agent %q --bind %q`, at.Spec.ID, formatBinding(b))
		var out string
		err := common.WithConfigMutationLock(m.Name, func() error {
			var runErr error
			out, runErr = bash.RunOutput(client, script)
			return runErr
		})
		if err != nil {
			return fmt.Errorf("configure-bindings: bind %s: %w\n%s", formatBinding(b), err, out)
		}
	}

	return nil
}

func (s *ConfigureBindingsStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	if len(at.Spec.Bindings) == 0 {
		return nil
	}
	m := at.Machine
	host := common.ResolveMachineHost(ctx, m)
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-bindings verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	current, err := s.reader.AgentBindings(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
	if err != nil {
		return fmt.Errorf("configure-bindings verify: %w", err)
	}

	toAdd, toRemove := diffBindings(current, at.Spec.Bindings)
	if len(toAdd) > 0 || len(toRemove) > 0 {
		return fmt.Errorf("configure-bindings verify: %d missing, %d extra bindings", len(toAdd), len(toRemove))
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func formatBinding(b manifestdata.AgentBinding) string {
	if b.Account != "" {
		return b.Channel + ":" + b.Account
	}
	return b.Channel
}

func bindingKey(channel, account string) string {
	if account != "" {
		return channel + ":" + account
	}
	return channel
}

// diffBindings returns bindings that need to be added and removed to reach
// the desired state.
func diffBindings(current []common.RemoteBinding, desired []manifestdata.AgentBinding) (toAdd []manifestdata.AgentBinding, toRemove []manifestdata.AgentBinding) {
	currentSet := make(map[string]bool, len(current))
	for _, b := range current {
		currentSet[bindingKey(b.Channel, b.Account)] = true
	}

	desiredSet := make(map[string]bool, len(desired))
	for _, b := range desired {
		k := bindingKey(b.Channel, b.Account)
		desiredSet[k] = true
		if !currentSet[k] {
			toAdd = append(toAdd, b)
		}
	}

	for _, b := range current {
		k := bindingKey(b.Channel, b.Account)
		if !desiredSet[k] {
			toRemove = append(toRemove, manifestdata.AgentBinding{
				Channel: b.Channel,
				Account: b.Account,
			})
		}
	}
	return
}
