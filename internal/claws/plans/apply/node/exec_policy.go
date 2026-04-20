package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// ExecPolicyStep reconciles the node's execution policy (security + ask) via
// `openclaw exec-policy set`. This is the node-side gate that controls whether
// exec tool calls are accepted at all, independent of the gateway config.
type ExecPolicyStep struct {
	dial SSHDialFunc
}

func NewExecPolicyStep(opts Options) *ExecPolicyStep {
	return &ExecPolicyStep{dial: opts.SSHDial}
}

func (*ExecPolicyStep) Name() string { return "exec-policy" }

func (s *ExecPolicyStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	nt, ok := t.Payload.(*NodeTarget)
	if !ok {
		return false, nil
	}
	return nt.Spec.ExecPolicy != nil, nil
}

func (s *ExecPolicyStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	nt := t.Payload.(*NodeTarget)
	if nt.Spec.ExecPolicy == nil {
		return true, nil
	}
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", m.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	haveSec, haveAsk := readExecPolicy(client)
	wantSec := nt.Spec.ExecPolicy.Security
	wantAsk := nt.Spec.ExecPolicy.Ask

	if wantSec != "" && haveSec != wantSec {
		return false, nil
	}
	if wantAsk != "" && haveAsk != wantAsk {
		return false, nil
	}
	return true, nil
}

func (s *ExecPolicyStep) Execute(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	if nt.Spec.ExecPolicy == nil {
		return nil
	}
	m := nt.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("exec-policy: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	var args []string
	if nt.Spec.ExecPolicy.Security != "" {
		args = append(args, fmt.Sprintf("--security %s", nt.Spec.ExecPolicy.Security))
	}
	if nt.Spec.ExecPolicy.Ask != "" {
		args = append(args, fmt.Sprintf("--ask %s", nt.Spec.ExecPolicy.Ask))
	}
	if len(args) == 0 {
		return nil
	}

	script := fmt.Sprintf(`openclaw exec-policy set %s`, strings.Join(args, " "))
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("exec-policy: %w\n%s", err, out)
	}
	return nil
}

func (s *ExecPolicyStep) Verify(ctx context.Context, t scaffold.Target) error {
	nt := t.Payload.(*NodeTarget)
	if nt.Spec.ExecPolicy == nil {
		return nil
	}
	m := nt.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("exec-policy verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	haveSec, haveAsk := readExecPolicy(client)
	wantSec := nt.Spec.ExecPolicy.Security
	wantAsk := nt.Spec.ExecPolicy.Ask

	if wantSec != "" && haveSec != wantSec {
		return fmt.Errorf("exec-policy verify: security %q, want %q", haveSec, wantSec)
	}
	if wantAsk != "" && haveAsk != wantAsk {
		return fmt.Errorf("exec-policy verify: ask %q, want %q", haveAsk, wantAsk)
	}
	return nil
}

func readExecPolicy(client *xssh.Client) (security, ask string) {
	secOut, err := bash.RunOutput(client, `openclaw config get tools.exec.security 2>/dev/null || echo ""`)
	if err == nil {
		security = strings.TrimSpace(secOut)
	}
	askOut, err := bash.RunOutput(client, `openclaw config get tools.exec.ask 2>/dev/null || echo ""`)
	if err == nil {
		ask = strings.TrimSpace(askOut)
	}
	return
}
