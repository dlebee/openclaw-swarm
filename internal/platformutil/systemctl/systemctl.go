package systemctl

import (
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

func validateUnit(unit string) error {
	if strings.TrimSpace(unit) == "" {
		return fmt.Errorf("systemctl: empty unit name")
	}
	return nil
}

// Enable enables a systemd unit so it starts on boot.
func Enable(client *xssh.Client, unit string) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	script := fmt.Sprintf(`set -euo pipefail
sudo systemctl enable %s 2>/dev/null || true
`, unit)
	return sshutil.RunBashStdin(client, script)
}

// EnableNow enables and immediately starts a systemd unit.
func EnableNow(client *xssh.Client, unit string) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	script := fmt.Sprintf(`set -euo pipefail
sudo systemctl enable --now %s
`, unit)
	return sshutil.RunBashStdin(client, script)
}

// Restart restarts a systemd unit.
func Restart(client *xssh.Client, unit string) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	script := fmt.Sprintf(`set -euo pipefail
sudo systemctl restart %s
`, unit)
	return sshutil.RunBashStdin(client, script)
}

// IsActive reports whether a systemd unit is currently active (running).
func IsActive(client *xssh.Client, unit string) (bool, error) {
	if err := validateUnit(unit); err != nil {
		return false, err
	}
	script := fmt.Sprintf(`set -uo pipefail
if systemctl is-active --quiet %s 2>/dev/null; then
  echo active
else
  echo inactive
fi
`, unit)
	out, err := sshutil.RunBashStdinOutput(client, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "active", nil
}
