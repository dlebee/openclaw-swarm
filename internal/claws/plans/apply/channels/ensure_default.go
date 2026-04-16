package channels

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnsureDefaultStep sets `channels.<kind>.defaultAccount` for each channel
// kind that has a channel marked `default: true`. If no channel is marked
// default, the first channel of that kind is used.
type EnsureDefaultStep struct {
	dial SSHDialFunc
}

func NewEnsureDefaultStep(opts Options) *EnsureDefaultStep {
	return &EnsureDefaultStep{dial: opts.SSHDial}
}

func (*EnsureDefaultStep) Name() string { return "ensure-default-account" }

func (s *EnsureDefaultStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	ct, ok := t.Payload.(*ChannelTarget)
	if !ok {
		return false, nil
	}
	return len(ct.Channels) > 0, nil
}

// desiredDefaults returns kind -> accountName for each kind's default.
func desiredDefaults(channels []manifestdata.Channel) map[string]string {
	firsts := map[string]string{}
	explicits := map[string]string{}
	for _, ch := range channels {
		kind := string(ch.Kind)
		if _, ok := firsts[kind]; !ok {
			firsts[kind] = ch.Name
		}
		if ch.Default {
			explicits[kind] = ch.Name
		}
	}
	result := make(map[string]string, len(firsts))
	for kind, first := range firsts {
		if name, ok := explicits[kind]; ok {
			result[kind] = name
		} else {
			result[kind] = first
		}
	}
	return result
}

func (s *EnsureDefaultStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	ct := t.Payload.(*ChannelTarget)
	m := ct.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	for kind, wantName := range desiredDefaults(ct.Channels) {
		current, err := ReadDefaultAccount(client, kind)
		if err != nil || current != wantName {
			return false, nil
		}
	}
	return true, nil
}

func (s *EnsureDefaultStep) Execute(ctx context.Context, t scaffold.Target) error {
	ct := t.Payload.(*ChannelTarget)
	m := ct.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("ensure-default-account: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	for kind, name := range desiredDefaults(ct.Channels) {
		script := fmt.Sprintf(`openclaw config set "channels.%s.defaultAccount" %q`, kind, name)
		if err := RunWithConflictRetry(client, script); err != nil {
			return fmt.Errorf("ensure-default-account: %s: %w", kind, err)
		}
	}
	return nil
}

func (s *EnsureDefaultStep) Verify(ctx context.Context, t scaffold.Target) error {
	ct := t.Payload.(*ChannelTarget)
	m := ct.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("ensure-default-account verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	for kind, wantName := range desiredDefaults(ct.Channels) {
		current, err := ReadDefaultAccount(client, kind)
		if err != nil {
			return fmt.Errorf("ensure-default-account verify: %s: %w", kind, err)
		}
		if current != wantName {
			return fmt.Errorf("ensure-default-account verify: %s defaultAccount %q, want %q", kind, current, wantName)
		}
	}
	return nil
}
