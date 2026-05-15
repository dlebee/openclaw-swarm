// Package tmout provides helpers to detect and remove the TMOUT environment
// variable from shell profiles on a remote host over SSH. Some hosting
// providers (e.g. OVH) ship images with TMOUT set system-wide, which causes
// non-interactive SSH sessions to be killed mid-script by SIGALRM (exit 142).
package tmout

import (
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

const checkScript = `set -uo pipefail
grep -rE '^\s*TMOUT=' /etc/profile /etc/profile.d/ /root/.bashrc /root/.profile 2>/dev/null \
  && echo found || echo clean`

const unsetScript = `set -uo pipefail
for f in /etc/profile /etc/profile.d/*.sh /root/.bashrc /root/.profile; do
  [ -f "$f" ] || continue
  sudo sed -i '/^\s*TMOUT=/d' "$f"
done`

// IsSet reports whether a TMOUT assignment is present in any of the managed
// shell profiles on the remote host.
func IsSet(client *xssh.Client) (bool, error) {
	out, err := sshutil.RunBashStdinOutput(client, checkScript)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "found", nil
}

// Unset removes all TMOUT assignments from the managed shell profiles on the
// remote host. Idempotent and safe to call when TMOUT is already absent.
func Unset(client *xssh.Client) error {
	return sshutil.RunBashStdin(client, unsetScript)
}
