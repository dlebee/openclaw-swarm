package provisioning

import (
	"context"
	"fmt"
	"strings"
	"time"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshkeys"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
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

// Applicable implements scaffold.Step — Linode machines when SSH dialer and key are configured.
func (a *AuthorizeSSHKeyStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	_ = ctx
	if a.dial == nil || a.sshPubKey == "" {
		return false, nil
	}
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	if mt.Spec.Type != manifestdata.MachineTypeLinode {
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
	client, err := a.dial(ctx, host, sshPort(mt.Spec), sshLoginUser(mt.Spec))
	if err != nil {
		return false, nil
	}
	defer client.Close()
	if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(client, a.sshPubKey); err != nil {
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
	user := sshLoginUser(mt.Spec)

	var lastErr error
	for attempt := 0; attempt < 15; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		client, err := a.dial(ctx, host, port, user)
		if err == nil {
			err = sshkeys.AppendAuthorizedKeyLinePOSIX(client, a.sshPubKey)
			client.Close()
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
	user := sshLoginUser(mt.Spec)
	client, err := a.dial(ctx, host, port, user)
	if err != nil {
		return fmt.Errorf("authorize-ssh-key verify: dial: %w", err)
	}
	defer client.Close()
	if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(client, a.sshPubKey); err != nil {
		return fmt.Errorf("authorize-ssh-key verify: %w", err)
	}
	return nil
}

func sshPort(m manifestdata.Machine) int {
	if m.SSHPort == 0 {
		return 22
	}
	return m.SSHPort
}

func sshLoginUser(m manifestdata.Machine) string {
	u := strings.TrimSpace(m.SSHUser)
	if u == "" {
		return "root"
	}
	return u
}
