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

// Check implements scaffold.Step — dials the target and verifies the CLI's
// public key is present in authorized_keys.
//
// Connection errors (refused, timeout, reset) return (false, nil) — "not yet
// reachable, Execute will retry". This is intentional: a newly created Linode
// can take 1–3 minutes after status=running before sshd is listening, and
// returning an error here would abort the cell in the execution phase before
// Execute's retry loop has a chance to run. Treating "can't connect" as
// "unsatisfied" rather than "error" is the correct semantic: we don't know
// whether the key is present, so Execute must try.
//
// A key-mismatch (auth succeeded but key not found) is also NOT an error:
// it's the legitimate "unsatisfied, Execute will fix drift" verdict.
//
// Only errors AFTER a successful dial (e.g. sftp protocol failure) are
// propagated, because those indicate a real problem beyond "not ready yet".
func (a *AuthorizeSSHKeyStep) Check(ctx context.Context, t scaffold.Target) (satisfied bool, err error) {
	if a == nil || a.dial == nil || strings.TrimSpace(a.sshPubKey) == "" {
		return false, nil
	}
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	if mt.Instance == nil {
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
		// Connection not yet available — not an error, Execute will wait/retry.
		return false, nil
	}
	defer a.returnSSH(ctx, key, client)
	if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(client, a.sshPubKey); err != nil {
		// Key absent or mismatched. Not an error — Execute will fix drift.
		return false, nil
	}
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

	// Linode (and other hosted providers) can report status=running
	// before sshd is actually listening. Cloud-init and package
	// managers start in parallel with sshd, so the TCP port can stay
	// closed for up to 2–3 minutes on a cold image in a busy region.
	// 40 attempts × 5 s ≈ 3 min 20 s budget absorbs that worst case
	// without exceeding the per-phase timeout envelope.
	const (
		authorizeRetries  = 40
		authorizeRetryWait = 5 * time.Second
	)
	var lastErr error
	for attempt := 0; attempt < authorizeRetries; attempt++ {
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
		case <-time.After(authorizeRetryWait):
		}
	}
	return fmt.Errorf("authorize-ssh-key: dial %s@%s:%d after %d retries: %w", user, host, port, authorizeRetries, lastErr)
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
