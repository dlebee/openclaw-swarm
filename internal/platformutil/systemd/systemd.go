package systemd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

func validateUnit(unit string) error {
	if strings.TrimSpace(unit) == "" {
		return fmt.Errorf("systemd: empty unit name")
	}
	return nil
}

// ctlPrefix returns the systemctl invocation prefix and the XDG export line
// required for user-mode services.
func ctlPrefix(userMode bool) (prefix string, env string) {
	if userMode {
		return "systemctl --user", "export XDG_RUNTIME_DIR=/run/user/$(id -u)\n"
	}
	return "sudo systemctl", ""
}

// unitDir returns the directory where the unit file should be written.
func unitDir(userMode bool) string {
	if userMode {
		return "$HOME/.config/systemd/user"
	}
	return "/etc/systemd/system"
}

// Write renders the UnitSpec and writes the .service file on the remote host.
// For user-mode units, the user systemd directory is created if needed.
// After writing, daemon-reload is issued automatically.
func Write(client *xssh.Client, spec UnitSpec) error {
	if err := validateUnit(spec.Name); err != nil {
		return err
	}
	ctl, env := ctlPrefix(spec.UserMode)
	dir := unitDir(spec.UserMode)
	content := spec.Render()

	var script string
	if spec.UserMode {
		script = fmt.Sprintf(`set -euo pipefail
%smkdir -p %s
cat > %s/%s << 'UNITEOF'
%sUNITEOF
%s daemon-reload
`, env, dir, dir, spec.ServiceFileName(), content, ctl)
	} else {
		script = fmt.Sprintf(`set -euo pipefail
sudo tee %s/%s > /dev/null << 'UNITEOF'
%sUNITEOF
%s daemon-reload
`, dir, spec.ServiceFileName(), content, ctl)
	}
	return sshutil.RunBashStdin(client, script)
}

// Remove stops, disables, and removes a unit file from the remote host.
func Remove(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	dir := unitDir(userMode)
	svc := unit + ".service"
	script := fmt.Sprintf(`set -euo pipefail
%s%s stop %s 2>/dev/null || true
%s disable %s 2>/dev/null || true
rm -f %s/%s
%s daemon-reload
`, env, ctl, svc, ctl, svc, dir, svc, ctl)
	return sshutil.RunBashStdin(client, script)
}

// DaemonReload runs daemon-reload.
func DaemonReload(client *xssh.Client, userMode bool) error {
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -euo pipefail
%s%s daemon-reload
`, env, ctl)
	return sshutil.RunBashStdin(client, script)
}

// Enable enables a unit so it starts on boot.
func Enable(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -euo pipefail
%s%s enable %s 2>/dev/null || true
`, env, ctl, unit)
	return sshutil.RunBashStdin(client, script)
}

// EnableNow enables and immediately starts a unit.
func EnableNow(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -euo pipefail
%s%s enable --now %s
`, env, ctl, unit)
	return sshutil.RunBashStdin(client, script)
}

// Start starts a unit.
func Start(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -euo pipefail
%s%s start %s
`, env, ctl, unit)
	return sshutil.RunBashStdin(client, script)
}

// Stop stops a unit.
func Stop(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -euo pipefail
%s%s stop %s
`, env, ctl, unit)
	return sshutil.RunBashStdin(client, script)
}

// Restart restarts a unit.
func Restart(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -euo pipefail
%s%s restart %s
`, env, ctl, unit)
	return sshutil.RunBashStdin(client, script)
}

// IsActive reports whether a unit is currently active (running).
func IsActive(client *xssh.Client, unit string, userMode bool) (bool, error) {
	if err := validateUnit(unit); err != nil {
		return false, err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -uo pipefail
%sif %s is-active --quiet %s 2>/dev/null; then
  echo active
else
  echo inactive
fi
`, env, ctl, unit)
	out, err := sshutil.RunBashStdinOutput(client, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "active", nil
}

// Logs fetches recent journal entries for a unit.
func Logs(client *xssh.Client, unit string, userMode bool, lines int) (string, error) {
	if err := validateUnit(unit); err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 50
	}
	_, env := ctlPrefix(userMode)
	var jctl string
	if userMode {
		jctl = "journalctl --user"
	} else {
		jctl = "sudo journalctl"
	}
	script := fmt.Sprintf(`set -uo pipefail
%s%s -u %s -n %d --no-pager 2>/dev/null || true
`, env, jctl, unit, lines)
	return sshutil.RunBashStdinOutput(client, script)
}

// WaitActive polls IsActive until the unit is running or retries are exhausted.
// Useful after EnableNow or Start to handle the race where the command returns
// before the service is fully up.
func WaitActive(ctx context.Context, client *xssh.Client, unit string, userMode bool, retries int, delay time.Duration) error {
	if retries <= 0 {
		retries = 8
	}
	if delay <= 0 {
		delay = 2 * time.Second
	}
	for i := 0; i < retries; i++ {
		active, err := IsActive(client, unit, userMode)
		if err == nil && active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("systemd: %s not active after %d checks", unit, retries)
}

// EnableLingering enables user lingering so user services start without a login session.
func EnableLingering(client *xssh.Client, user string) error {
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("systemd: empty user for lingering")
	}
	script := fmt.Sprintf(`set -euo pipefail
sudo loginctl enable-linger %s
`, user)
	return sshutil.RunBashStdin(client, script)
}
