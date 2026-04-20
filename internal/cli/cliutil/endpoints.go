// Package cliutil holds small helpers shared by the `claws` sub-commands
// that live under internal/cli/commands. The pieces here are deliberately
// thin — anything meaty belongs under internal/claws/* — but CLI commands
// all need the same "resolve a manifest machine to a live SSH endpoint"
// glue, and keeping it in one place stops every command from reinventing
// the wheel (and forgetting to handle multipass / linode).
package cliutil

import (
	"context"
	"fmt"
	"strings"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// Endpoint is a fully-resolved SSH target for a manifest machine.
type Endpoint struct {
	// Machine is the manifest machine this endpoint was resolved for.
	// Callers who need cpus/memory/type/etc. can read it here instead
	// of re-walking m.Machines.
	Machine manifestdata.Machine
	// Host is the reachable IPv4 or DNS name. Guaranteed non-empty.
	Host string
	// Port is the SSH port (defaults to 22 when the manifest omits it).
	Port int
	// User is the login account. Precedence matches the apply pipeline:
	// agent_user → bootstrap_user → root.
	User string
}

// EndpointResolver answers "where's machine X right now?" for a manifest
// after any provider-side discovery has already been done. Obtain one via
// PrepareEndpoints; do not construct directly.
//
// The resolver is stateful in the sense that the first PrepareEndpoints
// call seeds a plan cache on the context it returns. Subsequent
// Resolve calls read from that cache, so they're O(1) and don't re-hit
// the hosting provider. Pass the enriched context back into anything
// downstream that expects common.ResolveMachineHost to work.
//
// Cache-purity invariant: PrepareEndpoints MUST both (a) eagerly warm the
// plan cache with a single provider ListByTag, AND (b) install a lazy
// HostResolverFn so that any subsequent common.ResolveMachineHost call
// on the same context is read-through — a cold cache and a hot cache
// produce the same answer (rule: "Cold cache == hot cache"). Without
// (b), Resolve was ordering-dependent on Prepare being called first; with
// it, ResolveMachineHost itself re-hydrates on miss.
type EndpointResolver struct {
	ctx context.Context
}

// PrepareEndpoints walks the manifest's machines and, for any backed by a
// hosting provider (multipass, linode), queries the provider once to
// discover their current PublicIPv4. Results are cached on the returned
// context so later Resolve calls — and any shared helpers that go through
// common.ResolveMachineHost — see them for free.
//
// In addition to the eager seed, a HostResolverFn is registered on the
// context so that common.ResolveMachineHost can lazily re-hydrate on a
// plan-cache miss. This makes the whole pipeline read-through: callers
// no longer need to remember to pair Prepare with Resolve.
//
// For SSH-type manifests (or manifests with no machines at all) this is
// a cheap no-op: the provider is nil and we skip straight to Resolve,
// which falls back to the static Host declared in the manifest.
//
// manifestAbsPath is the absolute path of the manifest file — it's only
// used to resolve relative env_file entries for the hosting provider's
// token (Linode). Pass the same value you handed to manifestsvc.LoadFile.
func PrepareEndpoints(
	ctx context.Context,
	manifestAbsPath string,
	m *manifestdata.Manifest,
) (context.Context, *EndpointResolver, error) {
	if m == nil {
		return ctx, nil, fmt.Errorf("cliutil: manifest is nil")
	}
	ctx = scaffold.EnsurePlanCache(ctx)

	provider, err := planapply.ProviderFromManifest(m, manifestAbsPath)
	if err != nil {
		return ctx, nil, err
	}
	if provider != nil {
		// Register the lazy resolver FIRST so that if the eager seed
		// fails we still fall back to a pure read-through. Once
		// registered the same ctx is "hydratable" from anywhere.
		prefix := m.Prefix
		machines := m.Machines
		common.RegisterHostResolver(ctx, func(ctx context.Context, machineName string) (string, bool, error) {
			if h, ok := scaffold.LookupPlanMachineHost(ctx, machineName); ok && h != "" {
				return h, true, nil
			}
			if _, done := scaffold.PlanCacheGet(ctx, planCacheEndpointsResolverDone); !done {
				targets := provisioning.BuildMachineTargets(machines)
				if err := provisioning.ResolveHostedInstances(ctx, provider, prefix, targets); err != nil {
					return "", false, err
				}
				scaffold.PlanCacheSet(ctx, planCacheEndpointsResolverDone, true)
			}
			if h, ok := scaffold.LookupPlanMachineHost(ctx, machineName); ok && h != "" {
				return h, true, nil
			}
			return "", false, nil
		})

		targets := provisioning.BuildMachineTargets(m.Machines)
		if err := provisioning.ResolveHostedInstances(ctx, provider, m.Prefix, targets); err != nil {
			return ctx, nil, err
		}
		scaffold.PlanCacheSet(ctx, planCacheEndpointsResolverDone, true)
	}
	return ctx, &EndpointResolver{ctx: ctx}, nil
}

// planCacheEndpointsResolverDone guards the lazy HostResolverFn registered
// by PrepareEndpoints against re-running ResolveHostedInstances for every
// cache miss. Namespaced separately from apply.go's key so the two entry
// points don't step on each other when a single process runs both flows
// (rare today, but keeps the contract clean).
const planCacheEndpointsResolverDone = "CLIUTIL_ENDPOINTS_RESOLVER_DONE"

// Resolve returns the effective SSH endpoint for m. Resolution order:
//
//  1. Plan-cache entry seeded by PrepareEndpoints (hosted VM live IPv4).
//  2. Static Host field from the manifest (SSH-type machines).
//
// When neither is available the machine is almost certainly a hosted VM
// that hasn't been provisioned yet — we emit a hint pointing the operator
// at `claws apply`, which is the only command that creates hosted machines
// in the first place.
func (r *EndpointResolver) Resolve(m manifestdata.Machine) (Endpoint, error) {
	if r == nil {
		return Endpoint{}, fmt.Errorf("cliutil: nil EndpointResolver (call PrepareEndpoints first)")
	}
	host := strings.TrimSpace(common.ResolveMachineHost(r.ctx, m))
	if host == "" {
		if manifestdata.IsHostedMachineType(m.Type) {
			return Endpoint{}, fmt.Errorf(
				"machine %q (%s) has no live host — is the VM running? run `claws apply` to create it",
				m.Name, m.Type)
		}
		return Endpoint{}, fmt.Errorf("machine %q has no host", m.Name)
	}
	return Endpoint{
		Machine: m,
		Host:    host,
		Port:    common.MachineSSHPort(m),
		User:    preferredUser(m),
	}, nil
}

// Context exposes the cache-enriched context so callers that also want to
// call common.ResolveMachineHost / BorrowSSH / etc. downstream get the
// same resolved-host view without an extra provider round-trip.
func (r *EndpointResolver) Context() context.Context {
	if r == nil {
		return context.Background()
	}
	return r.ctx
}

// preferredUser mirrors the pre-existing ad-hoc logic that every CLI
// command re-implemented inline: prefer the agent user (ops-time identity),
// then the bootstrap user (privileged install-time identity), then root
// as a last-ditch default. We deliberately don't call MachineAgentUser /
// MachineBootstrapUser directly because those helpers collapse the
// fallback into a single "this role or root" semantics — CLI commands
// historically tried both roles in order, so preserve that here for
// backwards compatibility with existing SSH-only manifests that declared
// bootstrap_user but no agent_user.
func preferredUser(m manifestdata.Machine) string {
	if u := strings.TrimSpace(m.AgentUser); u != "" {
		return u
	}
	if u := strings.TrimSpace(m.BootstrapUser); u != "" {
		return u
	}
	return "root"
}
