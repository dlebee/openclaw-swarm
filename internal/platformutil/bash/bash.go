package bash

import (
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/perflog"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

func sshRemote(client *xssh.Client) string {
	if client == nil {
		return "(unknown)"
	}
	if ra := client.RemoteAddr(); ra != nil {
		return ra.String()
	}
	return "(unknown)"
}

// Run executes a bash script on the remote host. The script is piped to
// /bin/bash -s via stdin. Returns a non-nil error wrapping stderr on failure.
func Run(client *xssh.Client, script string) error {
	if !perflog.Enabled() || !perflog.ScriptHasOpenclawCLI(script) {
		return sshutil.RunBashStdin(client, script)
	}
	start := time.Now()
	err := sshutil.RunBashStdin(client, script)
	perflog.Log(sshRemote(client), perflog.Summarize(script), time.Since(start), err)
	return err
}

// RunOutput executes a bash script and returns stdout as a string.
func RunOutput(client *xssh.Client, script string) (string, error) {
	if !perflog.Enabled() || !perflog.ScriptHasOpenclawCLI(script) {
		return sshutil.RunBashStdinOutput(client, script)
	}
	start := time.Now()
	out, err := sshutil.RunBashStdinOutput(client, script)
	perflog.Log(sshRemote(client), perflog.Summarize(script), time.Since(start), err)
	return out, err
}
