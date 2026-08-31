package node

import (
	"context"
	"encoding/json"
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
	dial         SSHDialFunc
	hostResolver common.HostResolverFn
}

func NewExecPolicyStep(opts Options) *ExecPolicyStep {
	return &ExecPolicyStep{dial: opts.SSHDial, hostResolver: opts.HostResolver}
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
	host, ok := common.HostKnown(ctx, m, s.hostResolver)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
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

	script := common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw exec-policy set %s`, strings.Join(args, " "))
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

// readExecPolicy reads the node's requested exec security + ask.
//
// `exec-policy set` (used by Execute) is the local convenience command that
// keeps `tools.exec.*` config and the local approvals document in sync. On
// OpenClaw 2026.8.1 the requested value may land under a per-agent path
// (agents.entries.*.tools.exec.security) rather than the top-level
// tools.exec.security, so a raw `config get tools.exec.security` can read
// back empty even though the policy was set correctly — which surfaced as
// `exec-policy verify: security "", want "full"`.
//
// We therefore prefer `exec-policy show --json`, which reports the same
// requested/host/effective policy facts on both versions, and fall back to
// the raw config paths only when the JSON surface is unavailable (older
// CLIs, or a machine where the command errors).
func readExecPolicy(client *xssh.Client) (security, ask string) {
	if sec, a, ok := readExecPolicyJSON(client); ok {
		return sec, a
	}
	secOut, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw config get tools.exec.security 2>/dev/null || echo ""`)
	if err == nil {
		security = strings.TrimSpace(secOut)
	}
	askOut, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw config get tools.exec.ask 2>/dev/null || echo ""`)
	if err == nil {
		ask = strings.TrimSpace(askOut)
	}
	return
}

// readExecPolicyJSON parses `openclaw exec-policy show --json`. The command
// returns a JSON object carrying requested / host / effective policy facts.
// We read the *requested* tools.exec policy (what claws asked for via
// exec-policy set), which is the drift target the verify compares against.
// Returns ok=false when the command is unavailable or unparseable so the
// caller can fall back to raw config reads.
func readExecPolicyJSON(client *xssh.Client) (security, ask string, ok bool) {
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw exec-policy show --json 2>/dev/null`)
	if err != nil {
		return "", "", false
	}
	raw := strings.TrimSpace(out)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		raw = raw[i:]
	} else {
		return "", "", false
	}
	// Shape (2026.8.1): {"requested":{"security":"full","ask":"off",...},
	//                    "host":{...},"effective":{...}}
	// Be liberal: accept a flat {security,ask} too, in case a version
	// reports the requested policy at the top level.
	var parsed struct {
		Security  string `json:"security"`
		Ask       string `json:"ask"`
		Requested struct {
			Security string `json:"security"`
			Ask      string `json:"ask"`
		} `json:"requested"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", "", false
	}
	security = parsed.Requested.Security
	ask = parsed.Requested.Ask
	if security == "" {
		security = parsed.Security
	}
	if ask == "" {
		ask = parsed.Ask
	}
	// Only claim success if we actually got a security value; otherwise let
	// the caller fall back to raw config reads.
	if strings.TrimSpace(security) == "" {
		return "", "", false
	}
	return strings.TrimSpace(security), strings.TrimSpace(ask), true
}
