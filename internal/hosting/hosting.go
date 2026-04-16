package hosting

import "context"

const KindLinode = "linode"

// CreateInstanceOpts configures a new cloud instance.
type CreateInstanceOpts struct {
	Label      string
	Region     string
	SKU        string
	Image      string
	Tags       []string
	PublicKeys []string
	RootPass   string
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
