package bash

import (
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

// Run executes a bash script on the remote host. The script is piped to
// /bin/bash -s via stdin. Returns a non-nil error wrapping stderr on failure.
func Run(client *xssh.Client, script string) error {
	return sshutil.RunBashStdin(client, script)
}

// RunOutput executes a bash script and returns stdout as a string.
func RunOutput(client *xssh.Client, script string) (string, error) {
	return sshutil.RunBashStdinOutput(client, script)
}
