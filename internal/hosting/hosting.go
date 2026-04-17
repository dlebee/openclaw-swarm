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
