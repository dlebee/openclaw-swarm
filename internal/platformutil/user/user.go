package user

import (
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

func validateUsername(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("user: empty username")
	}
	if name == "root" {
		return fmt.Errorf("user: refusing to manage root")
	}
	return nil
}

// Exists reports whether a Linux user account exists on the remote host.
func Exists(client *xssh.Client, name string) (bool, error) {
	if err := validateUsername(name); err != nil {
		return false, err
	}
	script := fmt.Sprintf(`set -uo pipefail
if id -u %s >/dev/null 2>&1; then echo exists; else echo missing; fi
`, name)
	out, err := sshutil.RunBashStdinOutput(client, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "exists", nil
}

// Ensure creates a user with a home directory and /bin/bash shell if it does
// not already exist, copies root's authorized_keys so the same SSH identity
// can connect as the new user, and grants passwordless sudo.
// Idempotent: safe to call repeatedly.
func Ensure(client *xssh.Client, name string) error {
	if err := validateUsername(name); err != nil {
		return err
	}
	script := fmt.Sprintf(`set -euo pipefail
id -u %[1]s >/dev/null 2>&1 || sudo useradd -m -s /bin/bash %[1]s
sudo mkdir -p /home/%[1]s/.ssh
sudo cp ~/.ssh/authorized_keys /home/%[1]s/.ssh/authorized_keys
sudo chown -R %[1]s:%[1]s /home/%[1]s/.ssh
sudo chmod 700 /home/%[1]s/.ssh
sudo chmod 600 /home/%[1]s/.ssh/authorized_keys
echo '%[1]s ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/%[1]s >/dev/null
sudo chmod 440 /etc/sudoers.d/%[1]s
sudo loginctl enable-linger %[1]s 2>/dev/null || true
`, name)
	return sshutil.RunBashStdin(client, script)
}
