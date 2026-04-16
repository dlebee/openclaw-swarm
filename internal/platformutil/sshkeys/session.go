package sshkeys

import (
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

func runBashStdin(client *xssh.Client, script string) error {
	return sshutil.RunBashStdin(client, script)
}
