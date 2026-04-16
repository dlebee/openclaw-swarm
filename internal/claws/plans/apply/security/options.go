package security

import (
	"context"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc func(ctx context.Context, host string, port int, user string) (*xssh.Client, error)

// Options configures the security phase.
type Options struct {
	SSHDial SSHDialFunc
}

func sshPool(ctx context.Context) *clawssh.Pool {
	return provisioning.SSHPool(ctx)
}
