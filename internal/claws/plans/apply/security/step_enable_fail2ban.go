package security

import (
	"context"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemctl"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// fail2banJailLocal is a minimal jail.local that enables the sshd jail.
// Without at least one enabled jail, fail2ban exits immediately on Debian 12+.
const fail2banJailLocal = `[sshd]
enabled  = true
port     = ssh
filter   = sshd
maxretry = 5
bantime  = 600
`

// EnableFail2banStep enables and starts fail2ban via systemctl.
type EnableFail2banStep struct {
	dial SSHDialFunc
}

func NewEnableFail2banStep(opts Options) *EnableFail2banStep {
	return &EnableFail2banStep{dial: opts.SSHDial}
}

func (*EnableFail2banStep) Name() string { return "enable-fail2ban" }

func (s *EnableFail2banStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := isLinodeMachine(t.Payload)
	return ok, nil
}

func (s *EnableFail2banStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := isLinodeMachine(t.Payload)
	if !ok {
		return false, nil
	}
	host := machineHost(mt)
	if host == "" {
		return false, nil
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineSSHUser(mt.Spec))
	if err != nil {
		return false, nil
	}
	defer returnSSH(ctx, key, client)

	active, err := systemctl.IsActive(client, "fail2ban")
	if err != nil || !active {
		return false, nil
	}
	return true, nil
}

func (s *EnableFail2banStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := isLinodeMachine(t.Payload)
	if !ok {
		return fmt.Errorf("enable-fail2ban: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(mt)
	if host == "" {
		return fmt.Errorf("enable-fail2ban: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSHWithRetry(ctx, s.dial, host, machineSSHPort(mt.Spec), machineSSHUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("enable-fail2ban: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := ensureFail2banJailLocal(client); err != nil {
		return fmt.Errorf("enable-fail2ban: write jail.local: %w", err)
	}
	if err := systemctl.EnableNow(client, "fail2ban"); err != nil {
		diag := diagFail2ban(client)
		return fmt.Errorf("enable-fail2ban: %w\n%s", err, diag)
	}
	return nil
}

// diagFail2ban collects journal + status output to help diagnose start failures.
func diagFail2ban(client *xssh.Client) string {
	out, err := runScriptOutput(client, `
systemctl status fail2ban 2>&1 || true
echo "---journal---"
journalctl -u fail2ban --no-pager -n 30 2>&1 || true
`)
	if err != nil {
		return "(could not collect diagnostics)"
	}
	return out
}

// ensureFail2banJailLocal writes /etc/fail2ban/jail.local only if it does not
// already exist, so that a user's customisation is preserved on re-runs.
func ensureFail2banJailLocal(client *xssh.Client) error {
	script := fmt.Sprintf(`set -euo pipefail
if [ ! -f /etc/fail2ban/jail.local ]; then
  cat > /etc/fail2ban/jail.local <<'JAIL'
%sJAIL
fi
`, fail2banJailLocal)
	return runScript(client, script)
}

func (s *EnableFail2banStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := isLinodeMachine(t.Payload)
	if !ok {
		return fmt.Errorf("enable-fail2ban verify: expected *MachineTarget for %q", t.ID)
	}
	host := machineHost(mt)
	if host == "" {
		return fmt.Errorf("enable-fail2ban verify: no reachable host for %q", t.ID)
	}
	client, key, err := borrowSSH(ctx, s.dial, host, machineSSHPort(mt.Spec), machineSSHUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("enable-fail2ban verify: dial: %w", err)
	}
	defer returnSSH(ctx, key, client)

	if err := waitServiceActive(ctx, client, "fail2ban"); err != nil {
		diag := diagFail2ban(client)
		return fmt.Errorf("enable-fail2ban verify: %w\n%s", err, diag)
	}
	return nil
}
