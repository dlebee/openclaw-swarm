package apt

import (
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

// Update runs apt-get update on the remote host.
func Update(client *xssh.Client) error {
	return sshutil.RunBashStdin(client, `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
`)
}

// Install installs one or more packages via apt-get. Idempotent.
func Install(client *xssh.Client, pkgs ...string) error {
	if len(pkgs) == 0 {
		return fmt.Errorf("apt: no packages specified")
	}
	for _, p := range pkgs {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("apt: empty package name")
		}
	}
	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get install -y -qq %s
`, strings.Join(pkgs, " "))
	return sshutil.RunBashStdin(client, script)
}

// IsInstalled reports whether a package is installed (dpkg status "ii").
func IsInstalled(client *xssh.Client, pkg string) (bool, error) {
	if strings.TrimSpace(pkg) == "" {
		return false, fmt.Errorf("apt: empty package name")
	}
	script := fmt.Sprintf(`set -euo pipefail
dpkg -l %s 2>/dev/null | grep -q '^ii' && echo installed || echo not-installed
`, pkg)
	out, err := sshutil.RunBashStdinOutput(client, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "installed", nil
}
