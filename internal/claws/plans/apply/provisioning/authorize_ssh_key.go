package provisioning

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// AuthorizeSSHKeyAction ensures the active CLI public key is present in the target user's authorized_keys.
// It runs after create-machine (separate step) so SSH is only attempted once the host has an address.
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

// Applicable implements scaffold.Action — Linode rows with a reachable instance and configured dialer/key.
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
	if mt.Instance == nil || strings.TrimSpace(mt.Instance.PublicIPv4) == "" {
		return false, nil
	}
	return true, nil
}

// Check implements scaffold.Action.
func (*AuthorizeSSHKeyAction) Check(ctx context.Context, t scaffold.Target) (blocked bool, err error) {
	_ = ctx
	_ = t
	return false, nil
}

// Execute implements scaffold.Action.
func (a *AuthorizeSSHKeyAction) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("authorize-ssh-key: expected *MachineTarget for target %q", t.ID)
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
			err = runRemoteBash(client, authorizeKeyScript(a.sshPubKey))
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
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	port := sshPort(mt.Spec)
	user := sshLoginUser(mt.Spec)
	client, err := a.dial(ctx, host, port, user)
	if err != nil {
		return fmt.Errorf("authorize-ssh-key verify: dial: %w", err)
	}
	defer client.Close()
	if err := runRemoteBash(client, verifyKeyScript(a.sshPubKey)); err != nil {
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

func authorizeKeyScript(pubKey string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(pubKey)))
	q := "'" + strings.ReplaceAll(enc, "'", "'\\''") + "'"
	return fmt.Sprintf(`set -euo pipefail
KEY_LINE=$(printf '%%s' %s | base64 -d)
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
AUTH="$HOME/.ssh/authorized_keys"
touch "$AUTH"
chmod 600 "$AUTH"
grep -qxF -- "$KEY_LINE" "$AUTH" 2>/dev/null && exit 0
printf '%%s\n' "$KEY_LINE" >> "$AUTH"
`, q)
}

func verifyKeyScript(pubKey string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(pubKey)))
	q := "'" + strings.ReplaceAll(enc, "'", "'\\''") + "'"
	return fmt.Sprintf(`set -euo pipefail
KEY_LINE=$(printf '%%s' %s | base64 -d)
AUTH="$HOME/.ssh/authorized_keys"
test -f "$AUTH"
grep -qxF -- "$KEY_LINE" "$AUTH"
`, q)
}

func runRemoteBash(client *xssh.Client, script string) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run("/bin/bash -s"); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
