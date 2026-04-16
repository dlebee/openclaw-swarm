package ufw

import (
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

// IsActive reports whether UFW is enabled and active on the remote host.
func IsActive(client *xssh.Client) (bool, error) {
	out, err := sshutil.RunBashStdinOutput(client, `set -euo pipefail
sudo ufw status 2>/dev/null || echo "inactive"
`)
	if err != nil {
		return false, err
	}
	return strings.Contains(strings.ToLower(out), "status: active"), nil
}

// AllowPort adds a UFW allow rule for the given port and protocol (e.g. "tcp").
func AllowPort(client *xssh.Client, port int, proto string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("ufw: invalid port %d", port)
	}
	proto = strings.TrimSpace(proto)
	if proto == "" {
		proto = "tcp"
	}
	script := fmt.Sprintf(`set -euo pipefail
sudo ufw allow %d/%s
`, port, proto)
	return sshutil.RunBashStdin(client, script)
}

// Enable enables UFW. The "y" pipe answers the interactive confirmation prompt.
// Idempotent: returns nil if UFW is already active.
func Enable(client *xssh.Client) error {
	return sshutil.RunBashStdin(client, `set -euo pipefail
echo y | sudo ufw enable >/dev/null 2>&1 || true
`)
}
