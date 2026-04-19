// Package multipass implements a hosting.Provider backed by the `multipass`
// CLI. It is the local-VM counterpart to internal/hosting/linode and is the
// foundation for the Multipass integration test tier described in
// docs/multipass-integration-plan.md.
//
// Design notes
//
//   - All CLI invocations go through the injected Runner. That's the seam
//     unit tests use to avoid requiring a real multipass install.
//   - Multipass has no tag primitive, so "tags" are sidecar JSON files keyed
//     by VM label under ~/.cache/openclaw/multipass/tags. See tags.go.
//   - "ResourceID" is the Multipass label (== spec.MachineLabel). That
//     matches Multipass's own identity model; `multipass delete <label>`
//     is the real-world teardown command.
//
// Non-goals
//
//   - Multi-host coordination. VMs launched on machine A are not visible
//     from machine B, and the sidecar tag store doesn't try to fix that.
//   - Snapshot / restore. Tests throw VMs away on teardown; there's no
//     state worth preserving.
package multipass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

// Provider is the hosting.Provider implementation.
type Provider struct {
	runner Runner
	tags   *tagStore

	// waitPollInterval controls how often WaitRunning polls `multipass info`.
	// Defaulted (not exported) because overriding it is only ever needed in
	// tests, which construct via NewForTesting.
	waitPollInterval time.Duration

	// launchImage is the Ubuntu release identifier passed to `multipass
	// launch`. Defaults to "24.04" — matching what manifests target today.
	// If the manifest sets Machine.Image (e.g. "22.04"), that wins.
	launchImage string
}

// Options configures NewProvider. All fields are optional.
type Options struct {
	// Runner overrides the CLI executor. Tests inject a fake; production
	// callers should leave this nil so NewExecRunner is used.
	Runner Runner
	// TagDir overrides the sidecar tag directory. Empty = default
	// ~/.cache/openclaw/multipass/tags. Mostly useful for tests that
	// want an isolated on-disk store per run.
	TagDir string
	// DefaultImage is the fallback Ubuntu release for `multipass launch`
	// when the per-machine Image is empty. Defaults to "24.04".
	DefaultImage string
}

// NewProvider constructs a Provider. Returns an error only for unrecoverable
// setup issues (can't create the tag directory). Missing multipass binary
// is NOT checked here — the first CLI call surfaces that, which lets callers
// build a Provider at startup without gating on local tooling.
func NewProvider(o Options) (*Provider, error) {
	runner := o.Runner
	if runner == nil {
		runner = NewExecRunner()
	}
	tags, err := newTagStore(o.TagDir)
	if err != nil {
		return nil, err
	}
	img := strings.TrimSpace(o.DefaultImage)
	if img == "" {
		img = "24.04"
	}
	return &Provider{
		runner:           runner,
		tags:             tags,
		waitPollInterval: 3 * time.Second,
		launchImage:      img,
	}, nil
}

// Kind implements hosting.Provider.
func (p *Provider) Kind() string { return hosting.KindMultipass }

// --------------------------------------------------------------------------
// CreateInstance
// --------------------------------------------------------------------------

// CreateInstance launches a Multipass VM named after opts.Label, seeds the
// supplied authorized keys via cloud-init, and records tags in the sidecar
// store. On success it returns a running Instance (state = "Running",
// PublicIPv4 populated) so the caller doesn't have to follow up with
// WaitRunning — matching Linode's semantics.
//
// We intentionally combine launch + wait here rather than splitting the way
// Linode does because `multipass launch` already blocks until the VM is up.
// A separate poll would just be sugar.
func (p *Provider) CreateInstance(ctx context.Context, opts hosting.CreateInstanceOpts) (*hosting.Instance, error) {
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		return nil, fmt.Errorf("multipass: label is required")
	}

	image := strings.TrimSpace(opts.Image)
	if image == "" {
		image = p.launchImage
	}
	args := []string{
		"launch",
		"--name", label,
		"--cloud-init", "-", // seed on stdin
	}
	if opts.CPUs > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", opts.CPUs))
	}
	if mem := strings.TrimSpace(opts.Memory); mem != "" {
		args = append(args, "--memory", mem)
	}
	if disk := strings.TrimSpace(opts.Disk); disk != "" {
		args = append(args, "--disk", disk)
	}
	args = append(args, image)

	seed := buildCloudInit(opts.PublicKeys, opts.BootstrapUser, opts.Hostname)
	if _, err := p.runner.Run(ctx, strings.NewReader(seed), args...); err != nil {
		return nil, fmt.Errorf("multipass launch %s: %w", label, err)
	}

	// Record tags BEFORE polling for IP — a crash between launch and the
	// tag save would otherwise leave an untagged VM that ListByTag can't
	// find. Destroy would leak it.
	if err := p.tags.save(label, opts.Tags); err != nil {
		// Non-fatal: the VM exists and is named; destroy-by-label still
		// works via `multipass delete`. Surface a warning via error so
		// callers decide, but return the Instance too so they can clean up.
		return p.pollInstance(ctx, label, opts.Tags), fmt.Errorf(
			"multipass launch %s succeeded but saving tags failed: %w", label, err)
	}

	inst := p.pollInstance(ctx, label, opts.Tags)
	return inst, nil
}

// pollInstance waits until `multipass info` shows an IPv4 (VMs transition
// from "Starting"→"Running"→IPv4-assigned very quickly but not atomically).
// Returns whatever it has when ctx is cancelled.
func (p *Provider) pollInstance(ctx context.Context, label string, tags []string) *hosting.Instance {
	for {
		inst, err := p.info(ctx, label)
		if err == nil && inst.PublicIPv4 != "" {
			inst.Tags = dedupSorted(tags)
			return inst
		}
		select {
		case <-ctx.Done():
			// Return best-effort instance — caller may still want to delete it.
			if inst == nil {
				inst = &hosting.Instance{Provider: hosting.KindMultipass, ResourceID: label, Label: label}
			}
			inst.Tags = dedupSorted(tags)
			return inst
		case <-time.After(p.waitPollInterval):
		}
	}
}

// --------------------------------------------------------------------------
// WaitRunning
// --------------------------------------------------------------------------

// WaitRunning implements hosting.Provider. For Multipass the launch call
// itself blocks until the VM is up, so this is mostly symmetry with the
// Linode interface — useful when a caller has an instance reference and
// wants to re-confirm it's reachable.
func (p *Provider) WaitRunning(ctx context.Context, resourceID string) (*hosting.Instance, error) {
	for {
		inst, err := p.info(ctx, resourceID)
		if err == nil && strings.EqualFold(inst.Status, "running") && inst.PublicIPv4 != "" {
			tags, terr := p.tags.load(resourceID)
			if terr == nil {
				inst.Tags = tags
			}
			return inst, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.waitPollInterval):
		}
	}
}

// --------------------------------------------------------------------------
// DeleteInstance
// --------------------------------------------------------------------------

// DeleteInstance runs `multipass delete <label> --purge` and removes the
// tag sidecar. Missing VMs are treated as success — destroy paths are
// routinely racy (two test runs cleaning up the same label) and "already
// gone" is the intended end state.
func (p *Provider) DeleteInstance(ctx context.Context, resourceID string) error {
	label := strings.TrimSpace(resourceID)
	if label == "" {
		return fmt.Errorf("multipass: resourceID is required for delete")
	}
	_, err := p.runner.Run(ctx, nil, "delete", label, "--purge")
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("multipass delete %s: %w", label, err)
	}
	if tagErr := p.tags.forget(label); tagErr != nil {
		return fmt.Errorf("multipass delete %s: forget tags: %w", label, tagErr)
	}
	return nil
}

// --------------------------------------------------------------------------
// ListByTag
// --------------------------------------------------------------------------

// ListByTag enumerates VMs whose sidecar tag store contains the requested
// tag. Matches Linode's semantics as far as destroy/automations are
// concerned: returns real instances (with live state + IPv4 from `multipass
// info`) only for currently-launched VMs.
//
// VMs that exist in multipass but not in our tag store are skipped — they
// were launched out-of-band and aren't ours to operate on.
func (p *Provider) ListByTag(ctx context.Context, tag string) ([]hosting.Instance, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, nil
	}
	labels, err := p.listLabels(ctx)
	if err != nil {
		return nil, err
	}
	var out []hosting.Instance
	for _, label := range labels {
		tags, err := p.tags.load(label)
		if err != nil {
			// A corrupt sidecar is not fatal for the other VMs — just skip.
			continue
		}
		if !matchesTag(tags, tag) {
			continue
		}
		inst, err := p.info(ctx, label)
		if err != nil {
			// VM disappeared between `list` and `info`. Treat as gone.
			continue
		}
		inst.Tags = tags
		out = append(out, *inst)
	}
	return out, nil
}

// --------------------------------------------------------------------------
// internal CLI helpers
// --------------------------------------------------------------------------

// info queries `multipass info <label> --format json` and returns a
// partially-populated Instance (no Tags; callers attach those). Returns a
// typed not-found error when the VM is missing so DeleteInstance can treat
// it as idempotent.
func (p *Provider) info(ctx context.Context, label string) (*hosting.Instance, error) {
	out, err := p.runner.Run(ctx, nil, "info", label, "--format", "json")
	if err != nil {
		if isNotFoundErr(err) {
			return nil, errInstanceNotFound
		}
		return nil, fmt.Errorf("multipass info %s: %w", label, err)
	}
	var doc struct {
		Info map[string]struct {
			State string   `json:"state"`
			IPv4  []string `json:"ipv4"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse multipass info: %w\n%s", err, out)
	}
	e, ok := doc.Info[label]
	if !ok {
		return nil, errInstanceNotFound
	}
	ip := ""
	if len(e.IPv4) > 0 {
		ip = strings.TrimSpace(e.IPv4[0])
	}
	return &hosting.Instance{
		Provider:   hosting.KindMultipass,
		ResourceID: label,
		Label:      label,
		PublicIPv4: ip,
		Status:     normalizeState(e.State),
	}, nil
}

// listLabels returns every VM name known to multipass, regardless of state.
// Used as the primary input to ListByTag.
func (p *Provider) listLabels(ctx context.Context) ([]string, error) {
	out, err := p.runner.Run(ctx, nil, "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("multipass list: %w", err)
	}
	var doc struct {
		List []struct {
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse multipass list: %w\n%s", err, out)
	}
	labels := make([]string, 0, len(doc.List))
	for _, e := range doc.List {
		if name := strings.TrimSpace(e.Name); name != "" {
			labels = append(labels, name)
		}
	}
	return labels, nil
}

// errInstanceNotFound is returned by info() when the VM doesn't exist.
// It is intentionally internal — external callers get an empty slice from
// ListByTag or a nil error from DeleteInstance instead.
var errInstanceNotFound = errors.New("multipass: instance not found")

// isNotFoundErr checks whether the multipass CLI complained about a missing
// instance. The CLI doesn't expose a structured error code so we scan the
// stderr message. Kept permissive — extra matches are fine, false positives
// would only mean we treat a weird error as "not found", which at worst
// delays a real failure by one retry.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "does not exist") ||
		strings.Contains(s, "instance not found") ||
		strings.Contains(s, "unknown instance")
}

// normalizeState maps Multipass's capitalized states ("Running", "Stopped",
// "Starting") to the lowercase strings the rest of the codebase expects
// from Linode ("running"). Keeps downstream comparisons (`status ==
// "running"`) provider-agnostic.
func normalizeState(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
