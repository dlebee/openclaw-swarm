package provisioning

import (
	"context"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	xssh "golang.org/x/crypto/ssh"
)

// SSHDialFunc opens an SSH client for authorize-ssh-key (active CLI identity).
type SSHDialFunc func(ctx context.Context, host string, port int, user string) (*xssh.Client, error)

// Options configures the provisioning phase.
type Options struct {
	Provider  hosting.Provider
	Prefix    string // manifest prefix for label generation
	SSHPubKey string // public key content for authorized_keys
	// SSHDial connects with the active CLI identity (same as apply). Used by authorize-ssh-key.
	SSHDial SSHDialFunc
}
