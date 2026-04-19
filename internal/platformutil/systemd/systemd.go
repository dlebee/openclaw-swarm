package systemd

import (
	"context"
	"fmt"
	"sort"
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
//
// Services that fail-fast during their initial settling window (e.g. a node
// daemon that exits while waiting for its gateway pairing to be approved)
// can hit systemd's default StartLimitBurst and land in
// `failed (Result: start-limit-hit)`. Once in that state, `systemctl
// restart` refuses with "Job for <unit>.service failed" until the failure
// counter is cleared. We preemptively `reset-failed` (no-op on healthy
// units) so a legitimate restart after the service's external precondition
// becomes satisfiable doesn't spuriously fail because of earlier self-
// induced restarts. Retry once on transient errors to absorb the narrow
// race where systemd is still tearing down the previous cgroup when we
// issue the restart.
func Restart(client *xssh.Client, unit string, userMode bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, env := ctlPrefix(userMode)
	script := fmt.Sprintf(`set -uo pipefail
%s%s reset-failed %s 2>/dev/null || true
if %s restart %s; then
  exit 0
fi
sleep 2
%s reset-failed %s 2>/dev/null || true
%s restart %s
`, env, ctl, unit, ctl, unit, ctl, unit, ctl, unit)
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

// dropInDir returns the drop-in directory for a unit's override fragments.
func dropInDir(unit string, userMode bool) string {
	svc := unit + ".service.d"
	if userMode {
		return "$HOME/.config/systemd/user/" + svc
	}
	return "/etc/systemd/system/" + svc
}

// WriteEnvDropIn writes (or overwrites) an environment drop-in override for an
// existing unit. The drop-in adds [Service] Environment= lines for each key in
// env. Pass an empty map to remove the drop-in. After writing, daemon-reload is
// issued automatically.
func WriteEnvDropIn(client *xssh.Client, unit string, userMode bool, env map[string]string) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	ctl, xdg := ctlPrefix(userMode)
	dir := dropInDir(unit, userMode)

	if len(env) == 0 {
		script := fmt.Sprintf(`set -euo pipefail
%srm -f %s/env.conf
%s daemon-reload
`, xdg, dir, ctl)
		return sshutil.RunBashStdin(client, script)
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var body strings.Builder
	body.WriteString("[Service]\n")
	for _, k := range keys {
		fmt.Fprintf(&body, "Environment=%s=%s\n", k, env[k])
	}

	var mkdirCmd string
	if userMode {
		mkdirCmd = fmt.Sprintf("mkdir -p %s", dir)
	} else {
		mkdirCmd = fmt.Sprintf("sudo mkdir -p %s", dir)
	}

	var writeCmd string
	if userMode {
		writeCmd = fmt.Sprintf("cat > %s/env.conf << 'ENVEOF'\n%sENVEOF", dir, body.String())
	} else {
		writeCmd = fmt.Sprintf("sudo tee %s/env.conf > /dev/null << 'ENVEOF'\n%sENVEOF", dir, body.String())
	}

	script := fmt.Sprintf(`set -euo pipefail
%s%s
%s
%s daemon-reload
`, xdg, mkdirCmd, writeCmd, ctl)
	return sshutil.RunBashStdin(client, script)
}

// ReadEnvDropIn reads the env.conf drop-in for a unit and returns the
// environment variables as a map. Returns an empty map if no drop-in exists.
func ReadEnvDropIn(client *xssh.Client, unit string, userMode bool) (map[string]string, error) {
	if err := validateUnit(unit); err != nil {
		return nil, err
	}
	_, xdg := ctlPrefix(userMode)
	dir := dropInDir(unit, userMode)

	script := fmt.Sprintf(`set -uo pipefail
%scat %s/env.conf 2>/dev/null || true
`, xdg, dir)
	out, err := sshutil.RunBashStdinOutput(client, script)
	if err != nil {
		return nil, err
	}

	env := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Environment=") {
			continue
		}
		kv := strings.TrimPrefix(line, "Environment=")
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			env[kv[:idx]] = kv[idx+1:]
		}
	}
	return env, nil
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
