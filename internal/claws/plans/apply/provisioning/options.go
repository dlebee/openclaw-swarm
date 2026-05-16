package provisioning

import (
	"context"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
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
	// UnsetTMOUT, when true, adds the unset-tmout step that removes the TMOUT
	// variable from shell profiles on the remote host. Enable for providers
	// (e.g. OVH) that ship images with TMOUT set, causing SSH sessions to be
	// killed mid-script with SIGALRM (exit 142).
	UnsetTMOUT bool
}

const planCacheSSHPoolKey = "SSH_POOL"

// SetSSHPool stores an SSH connection pool in the plan cache and registers it
// for cleanup when the plan finishes.
func SetSSHPool(ctx context.Context, pool *clawssh.Pool) {
	scaffold.PlanCacheSet(ctx, planCacheSSHPoolKey, pool)
	scaffold.RegisterPlanCloser(ctx, pool)
}

// SSHPool retrieves the SSH connection pool from the plan cache, or nil.
func SSHPool(ctx context.Context) *clawssh.Pool {
	v, ok := scaffold.PlanCacheGet(ctx, planCacheSSHPoolKey)
	if !ok {
		return nil
	}
	p, _ := v.(*clawssh.Pool)
	return p
}
