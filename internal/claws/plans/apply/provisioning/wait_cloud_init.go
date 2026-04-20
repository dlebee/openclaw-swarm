package provisioning

import (
	"context"
	"fmt"
	"strings"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/cloudinit"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// WaitCloudInitStep blocks until cloud-init finishes its first-boot work on
// hosted Ubuntu/Debian images.
//
// On fresh Ubuntu cloud images (Multipass, Linode, …) systemd triggers
// apt-daily.service / apt-daily-upgrade.service at boot. Those hold
// /var/lib/apt/lists/lock and /var/lib/dpkg/lock-frontend for the first
// ~30–120 s, racing every `apt-get update`/`install` a later phase issues
// (the symptom is `E: Could not get lock ... held by process N (apt-get)`
// with exit 100). cloud-init's final stage coordinates with apt-daily via
// the package-update-upgrade-install module, so `cloud-init status --wait`
// is a reliable "boot-time apt noise has settled" gate — after it returns,
// downstream phases' apt calls no longer race the daemon.
//
// When cloud-init is not present on the target (containers, minimal base
// images, hand-rolled SSH hosts), Check reports satisfied=true so Execute
// is skipped and the step is effectively a no-op. This keeps the step safe
// to enable unconditionally for every hosted machine without caring which
// distro / image variant the user picked.
type WaitCloudInitStep struct {
	dial SSHDialFunc
}

// NewWaitCloudInitStep builds the scaffold step from options.
func NewWaitCloudInitStep(opts Options) *WaitCloudInitStep {
	return &WaitCloudInitStep{dial: opts.SSHDial}
}

// Name implements scaffold.Step.
func (*WaitCloudInitStep) Name() string { return "wait-cloud-init" }

// Applicable implements scaffold.Step. Only hosted machine types with an
// SSH dialer configured — SSH-typed machines are pre-provisioned by the
// operator and already past their boot-time window by the time claws
// talks to them.
func (s *WaitCloudInitStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	if s == nil || s.dial == nil {
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

// Check implements scaffold.Step. Returns satisfied=true when cloud-init
// is absent on the host (nothing to wait for), letting the scaffold skip
// Execute entirely. A present cloud-init always falls through to Execute
// because there is no cheap way to tell "already fully settled" apart
// from "still settling" without running `status --wait` itself — and the
// wait is idempotent and fast when cloud-init has already finished, so
// re-running it is never wrong.
func (s *WaitCloudInitStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return false, nil
	}
	// No instance yet → unsatisfied, not an error. Applicable-style: a
	// downstream phase hasn't created the machine, so there's nothing to
	// probe. This is the ONLY remaining "false, nil on nil data" branch:
	// every other failure is propagated as an error so the probe UI can
	// render "check: ..." instead of lying as "will execute".
	if mt.Instance == nil {
		return false, nil
	}
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	if host == "" {
		return false, nil
	}
	client, key, err := s.borrowSSH(ctx, host, sshPort(mt.Spec), bootstrapLoginUser(mt.Spec))
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", host, err)
	}
	defer s.returnSSH(ctx, key, client)

	present, err := cloudinit.Has(client)
	if err != nil {
		return false, fmt.Errorf("detect cloud-init on %s: %w", host, err)
	}
	return !present, nil
}

// Execute implements scaffold.Step. Runs `cloud-init status --wait`, which
// blocks until cloud-init's final stage completes (or it has already
// completed on a prior boot, in which case the call returns immediately).
// Non-zero exit codes are tolerated inside cloudinit.Wait — `--wait`
// returns non-zero when cloud-init landed in an error state, but the
// caller's real interest is "the boot-time apt daemons are no longer
// holding the lock", which is true in every terminal state.
func (s *WaitCloudInitStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt, ok := t.Payload.(*MachineTarget)
	if !ok || mt == nil {
		return fmt.Errorf("wait-cloud-init: expected *MachineTarget for %q", t.ID)
	}
	if mt.Instance == nil || strings.TrimSpace(mt.Instance.PublicIPv4) == "" {
		return fmt.Errorf("wait-cloud-init: instance not ready for %q (no public IPv4)", t.ID)
	}
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	client, key, err := s.borrowSSH(ctx, host, sshPort(mt.Spec), bootstrapLoginUser(mt.Spec))
	if err != nil {
		return fmt.Errorf("wait-cloud-init: dial: %w", err)
	}
	defer s.returnSSH(ctx, key, client)

	present, err := cloudinit.Has(client)
	if err != nil {
		return fmt.Errorf("wait-cloud-init: detect: %w", err)
	}
	if !present {
		return nil
	}
	if err := cloudinit.Wait(client); err != nil {
		return fmt.Errorf("wait-cloud-init: %w", err)
	}
	return nil
}

// Verify implements scaffold.Step. Nothing to re-check beyond Execute.
func (*WaitCloudInitStep) Verify(_ context.Context, _ scaffold.Target) error { return nil }

func (s *WaitCloudInitStep) borrowSSH(ctx context.Context, host string, port int, user string) (*xssh.Client, string, error) {
	key := clawssh.HostKey(host, port, user)
	if pool := SSHPool(ctx); pool != nil {
		dial := s.dial
		c, err := pool.Borrow(ctx, key, func(ctx context.Context) (*xssh.Client, error) {
			return dial(ctx, host, port, user)
		})
		return c, key, err
	}
	c, err := s.dial(ctx, host, port, user)
	return c, key, err
}

func (s *WaitCloudInitStep) returnSSH(ctx context.Context, key string, c *xssh.Client) {
	if c == nil {
		return
	}
	if pool := SSHPool(ctx); pool != nil {
		pool.Return(key, c)
		return
	}
	c.Close()
}
