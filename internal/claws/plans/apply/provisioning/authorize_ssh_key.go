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

// AuthorizeSSHKeyAction ensures the active CLI public key is present in the target user's authorized_keys.
// It runs after create-machine (separate step) so SSH is only attempted once the host has an address.
// Use scaffold.DoesMachineExist(ctx, target.ID) when a probe result is needed; do not assume from a skipped phase.
type AuthorizeSSHKeyAction struct {
	dial      SSHDialFunc
	sshPubKey string
}

// NewAuthorizeSSHKeyAction builds the scaffold action from options.
func NewAuthorizeSSHKeyAction(opts Options) *AuthorizeSSHKeyAction {
	return &AuthorizeSSHKeyAction{
		dial:      opts.SSHDial,
		sshPubKey: strings.TrimSpace(opts.SSHPubKey),
	}
}

// Name implements scaffold.Action.
func (*AuthorizeSSHKeyAction) Name() string { return "authorize-ssh-key" }

// Applicable implements scaffold.Action — Linode machines when SSH dialer and key are configured.
// Instance/public IP are not required here: create-machine runs in an earlier step and may attach
// the instance before authorize-ssh-key's Check runs during the same plan walk.
func (a *AuthorizeSSHKeyAction) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
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

// Check implements scaffold.Action. When create-machine's Check has already attached an
// instance with a public IPv4, dial once and verify the CLI public key line is present;
// blocked=true skips Execute (same as create-machine when the instance already exists).
// Dial or verify errors do not block: Execute may still need to run to fix drift or reachability.
func (a *AuthorizeSSHKeyAction) Check(ctx context.Context, t scaffold.Target) (blocked bool, err error) {
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

// Execute implements scaffold.Action.
func (a *AuthorizeSSHKeyAction) Execute(ctx context.Context, t scaffold.Target) error {
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

// Verify implements scaffold.Action.
func (a *AuthorizeSSHKeyAction) Verify(ctx context.Context, t scaffold.Target) error {
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
