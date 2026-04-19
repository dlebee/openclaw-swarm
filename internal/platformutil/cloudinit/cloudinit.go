// Package cloudinit wraps cloud-init(1) status queries over SSH.
//
// cloud-init is the first-boot configuration tool shipped on virtually
// every mainstream cloud Linux image (Multipass, Linode, AWS, GCP, Azure,
// DigitalOcean, Hetzner, …). During its boot-time stages it coordinates
// with apt-daily / apt-daily-upgrade, which means `cloud-init status
// --wait` is a reliable gate for "the boot-time apt lock holders have
// released their locks" — something claws' provisioning phase cares
// about because the very next phase issues `apt-get update`.
package cloudinit

import (
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

// Has reports whether the cloud-init binary is on PATH on the remote
// host. Returns false without an error when cloud-init is absent — the
// caller is expected to treat "no cloud-init" as "nothing to wait for".
//
// `command -v` is used instead of `which` because `command` is a bash
// builtin and works reliably in a plain `/bin/bash -s` pipe (no login
// shell, no PATH surprises).
func Has(client *xssh.Client) (bool, error) {
	out, err := sshutil.RunBashStdinOutput(client, `command -v cloud-init >/dev/null 2>&1 && echo present || echo absent
`)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "present", nil
}

// Wait blocks until cloud-init's final stage has completed (or it had
// already completed on a prior boot, in which case the call returns
// immediately). A non-zero exit — cloud-init landed in an `error` or
// `degraded` terminal state — is intentionally tolerated: the caller's
// interest is "boot-time apt/dpkg locks are released", which is true in
// every terminal state. If the caller needs cloud-init health, they
// should call `cloud-init status` directly and inspect its output.
//
// The command is run under sudo because `cloud-init status` touches
// /run/cloud-init/* which is root-owned on current Ubuntu images.
func Wait(client *xssh.Client) error {
	return sshutil.RunBashStdin(client, `set -uo pipefail
sudo cloud-init status --wait >/dev/null 2>&1 || true
`)
}
