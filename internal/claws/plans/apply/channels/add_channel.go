package channels

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// AddChannelStep registers missing channel accounts on the gateway via
// `openclaw channels add`. Idempotent: skips accounts that already exist.
type AddChannelStep struct {
	dial SSHDialFunc
}

func NewAddChannelStep(opts Options) *AddChannelStep {
	return &AddChannelStep{dial: opts.SSHDial}
}

func (*AddChannelStep) Name() string { return "add-channels" }

func (s *AddChannelStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	ct, ok := t.Payload.(*ChannelTarget)
	if !ok {
		return false, nil
	}
	return len(ct.Channels) > 0, nil
}

func (s *AddChannelStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	ct := t.Payload.(*ChannelTarget)
	m := ct.Machine
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	accounts, err := ListChannelAccounts(client)
	if err != nil {
		return false, fmt.Errorf("list channel accounts on %s: %w", m.Name, err)
	}

	for _, ch := range ct.Channels {
		if !AccountExists(accounts, string(ch.Kind), ch.Name) {
			return false, nil
		}
	}
	return true, nil
}

func (s *AddChannelStep) Execute(ctx context.Context, t scaffold.Target) error {
	ct := t.Payload.(*ChannelTarget)
	m := ct.Machine
	host := common.ResolveMachineHost(ctx, m)
	port := common.MachineSSHPort(m)
	user := common.MachineAgentUser(m)

	// Listing is cheap and read-only; run it through the pool. Mutations
	// below go through RunWithConflictAndTransientRetry so that a dead
	// pooled session mid-command (seen as "exited without exit status")
	// triggers a fresh dial instead of failing the phase.
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, host, port, user)
	if err != nil {
		return fmt.Errorf("add-channels: %w", err)
	}
	accounts, lsErr := ListChannelAccounts(client)
	common.ReturnSSH(ctx, key, client)
	if lsErr != nil {
		accounts = ChannelAccounts{}
	}

	for _, ch := range ct.Channels {
		kind := string(ch.Kind)
		if AccountExists(accounts, kind, ch.Name) {
			continue
		}
		token := ct.Tokens[ch.TokenEnv]
		if token == "" {
			return fmt.Errorf("add-channels: token for %s (%s) is empty", ch.Name, ch.TokenEnv)
		}

		script := fmt.Sprintf(
			`openclaw channels add --channel %s --account %q --token %q`,
			kind, ch.Name, token)

		if err := RunWithConflictAndTransientRetry(ctx, s.dial, host, port, user, script); err != nil {
			return fmt.Errorf("add-channels: add %s/%s: %w", kind, ch.Name, err)
		}
	}
	return nil
}

func (s *AddChannelStep) Verify(ctx context.Context, t scaffold.Target) error {
	ct := t.Payload.(*ChannelTarget)
	m := ct.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("add-channels verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	accounts, err := ListChannelAccounts(client)
	if err != nil {
		return fmt.Errorf("add-channels verify: %w", err)
	}

	for _, ch := range ct.Channels {
		if !AccountExists(accounts, string(ch.Kind), ch.Name) {
			return fmt.Errorf("add-channels verify: %s/%s not found after add", ch.Kind, ch.Name)
		}
	}
	return nil
}
