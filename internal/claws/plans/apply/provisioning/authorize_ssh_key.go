package provisioning

import (
	"context"
	"fmt"
	"strings"
	"time"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshkeys"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// AuthorizeSSHKeyStep ensures the active CLI public key is present in the target
// user's authorized_keys. It runs after create-machine (later step in the same phase)
// so SSH is only attempted once the host has an address.
type AuthorizeSSHKeyStep struct {
	dial      SSHDialFunc
	sshPubKey string
}

// NewAuthorizeSSHKeyStep builds the scaffold step from options.
func NewAuthorizeSSHKeyStep(opts Options) *AuthorizeSSHKeyStep {
	return &AuthorizeSSHKeyStep{
		dial:      opts.SSHDial,
		sshPubKey: strings.TrimSpace(opts.SSHPubKey),
	}
}

// Name implements scaffold.Step.
func (*AuthorizeSSHKeyStep) Name() string { return "authorize-ssh-key" }

// Applicable implements scaffold.Step — hosted machines (linode, multipass,
// …) when an SSH dialer and a public key are configured. SSH-typed machines
// already have authorized_keys managed out-of-band and are skipped.
func (a *AuthorizeSSHKeyStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	_ = ctx
	if a.dial == nil || a.sshPubKey == "" {
		return false, nil
	}
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	if !manifestdata.IsHostedMachineType(mt.Spec.Type) {
		return false, nil
	}
	return true, nil
}

// Check implements scaffold.Step. When create-machine's Check has already attached an
// instance with a public IPv4, dial once and verify the CLI public key line is present;
// satisfied=true skips Execute. Dial or verify errors are not fatal: Execute may still
// need to run to fix drift or reachability.
func (a *AuthorizeSSHKeyStep) Check(ctx context.Context, t scaffold.Target) (satisfied bool, err error) {
	if a == nil || a.dial == nil || strings.TrimSpace(a.sshPubKey) == "" {
		return false, nil
	}
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil || mt.Instance == nil {
		return false, nil
	}
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	if host == "" {
		return false, nil
	}
	port := sshPort(mt.Spec)
	user := bootstrapLoginUser(mt.Spec)
	client, key, err := a.borrowSSH(ctx, host, port, user)
	if err != nil {
		return false, nil
	}
	if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(client, a.sshPubKey); err != nil {
		a.returnSSH(ctx, key, client)
		return false, nil
	}
	a.returnSSH(ctx, key, client)
	return true, nil
}

// Execute implements scaffold.Step.
func (a *AuthorizeSSHKeyStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("authorize-ssh-key: expected *MachineTarget for target %q", t.ID)
	}
	if mt.Instance == nil || strings.TrimSpace(mt.Instance.PublicIPv4) == "" {
		return fmt.Errorf("authorize-ssh-key: instance not ready for %q (no public IPv4); ensure create-machine ran for this target first", t.ID)
	}
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	port := sshPort(mt.Spec)
	user := bootstrapLoginUser(mt.Spec)

	var lastErr error
	for attempt := 0; attempt < 15; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		client, key, err := a.borrowSSH(ctx, host, port, user)
		if err == nil {
			err = sshkeys.AppendAuthorizedKeyLinePOSIX(client, a.sshPubKey)
			a.returnSSH(ctx, key, client)
			if err != nil {
				return fmt.Errorf("authorize-ssh-key: %w", err)
			}
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("authorize-ssh-key: dial %s@%s:%d after retries: %w", user, host, port, lastErr)
}

// Verify implements scaffold.Step.
func (a *AuthorizeSSHKeyStep) Verify(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("authorize-ssh-key verify: expected *MachineTarget for %q", t.ID)
	}
	if a.dial == nil {
		return fmt.Errorf("authorize-ssh-key verify: no SSH dialer")
	}
	if mt.Instance == nil || strings.TrimSpace(mt.Instance.PublicIPv4) == "" {
		return fmt.Errorf("authorize-ssh-key verify: instance not ready for %q (no public IPv4)", t.ID)
	}
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	port := sshPort(mt.Spec)
	user := bootstrapLoginUser(mt.Spec)
	client, key, err := a.borrowSSH(ctx, host, port, user)
	if err != nil {
		return fmt.Errorf("authorize-ssh-key verify: dial: %w", err)
	}
	if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(client, a.sshPubKey); err != nil {
		a.returnSSH(ctx, key, client)
		return fmt.Errorf("authorize-ssh-key verify: %w", err)
	}
	a.returnSSH(ctx, key, client)
	return nil
}

// borrowSSH gets a client from the plan-scoped SSH pool (or dials directly if
// no pool is registered). Returns the pool key for the subsequent Return call.
func (a *AuthorizeSSHKeyStep) borrowSSH(ctx context.Context, host string, port int, user string) (*xssh.Client, string, error) {
	key := clawssh.HostKey(host, port, user)
	if pool := SSHPool(ctx); pool != nil {
		dial := a.dial
		c, err := pool.Borrow(ctx, key, func(ctx context.Context) (*xssh.Client, error) {
			return dial(ctx, host, port, user)
		})
		return c, key, err
	}
	c, err := a.dial(ctx, host, port, user)
	return c, key, err
}

// returnSSH puts a client back into the pool (or closes it if no pool).
func (a *AuthorizeSSHKeyStep) returnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := SSHPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}

func sshPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

// bootstrapLoginUser mirrors common.MachineBootstrapUser. Provisioning
// deliberately avoids the common helper to keep its dependencies tight
// (provisioning is imported by common, not the other way around), but
// the semantics must stay in lockstep: BootstrapUser or "root".
func bootstrapLoginUser(m manifestdata.Machine) string {
	u := strings.TrimSpace(m.BootstrapUser)
	if u == "" {
		return "root"
	}
	return u
}
