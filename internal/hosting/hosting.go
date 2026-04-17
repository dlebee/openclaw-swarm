package hosting

import "context"

// Kind* are the provider identifiers returned by Provider.Kind(). They mirror
// the manifest's `type:` values for machines so call sites can match a
// provider to the machines it's responsible for without a second lookup.
const (
	KindLinode    = "linode"
	KindMultipass = "multipass"
)

// CreateInstanceOpts configures a new cloud/VM instance.
//
// Not every field is meaningful for every provider. Linode uses Region, SKU,
// Image, RootPass; Multipass uses Image, CPUs, Memory, Disk. Tags, Label and
// PublicKeys are universal. Providers MUST silently ignore fields that don't
// apply to them rather than erroring on unknown input — this keeps
// `CreateMachineStep.Execute` provider-agnostic.
type CreateInstanceOpts struct {
	Label      string
	Image      string
	Tags       []string
	PublicKeys []string

	// BootstrapUser is the Unix account the caller intends to SSH into
	// post-launch (for authorize-ssh-key, ensure-agent-user, security
	// hardening). Providers use it to decide where PublicKeys land in the
	// fresh image:
	//
	//   - Linode ignores it — its Ubuntu images already accept root via
	//     the RootPass + injected key flow.
	//   - Multipass seeds the default cloud-image user (ubuntu) always,
	//     and additionally enables root SSH + seeds root's authorized_keys
	//     when BootstrapUser == "root". Cloud-init locks root by default,
	//     so we must explicitly opt out via `disable_root: false`.
	//
	// Empty value is treated as "root" for backwards compat with existing
	// Linode manifests that never set this.
	BootstrapUser string

	// Hostname is the short, stable name the VM advertises on its LAN
	// once it's up. Providers that can honor it (Multipass via cloud
	// -init's `hostname:` directive + Avahi) use it to give peers a
	// predictable `<hostname>.local` address — so a manifest pointing at
	// `http://gateway-host.local:8080` keeps working across relaunches
	// where the LAN IP changes. Ignored by providers that can't control
	// VM hostnames post-launch (Linode).
	//
	// Empty means "provider's default" (Multipass falls back to the label,
	// which mixes in the manifest prefix and therefore changes per run).
	Hostname string

	// Linode-specific.
	Region   string
	SKU      string
	RootPass string

	// Multipass-specific. Zero/empty means "provider default".
	CPUs   int
	Memory string
	Disk   string
}

// Instance is a cloud instance record.
type Instance struct {
	Provider   string
	ResourceID string
	Label      string
	Region     string
	PublicIPv4 string
	Status     string
	Tags       []string
}

// Provider is a cloud machine provider (Linode, etc.).
type Provider interface {
	Kind() string
	CreateInstance(ctx context.Context, opts CreateInstanceOpts) (*Instance, error)
	DeleteInstance(ctx context.Context, resourceID string) error
	WaitRunning(ctx context.Context, resourceID string) (*Instance, error)
	// ListByTag returns instances that include the given Linode tag (exact string on the instance).
	ListByTag(ctx context.Context, tag string) ([]Instance, error)
}
