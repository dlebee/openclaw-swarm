package common

import (
	"context"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

func TestResolveMachineHost_cacheWins(t *testing.T) {
	ctx := scaffold.EnsurePlanCache(context.Background())
	m := manifestdata.Machine{Name: "gateway-host", Host: "fallback.example.com"}
	scaffold.RecordPlanMachineHost(ctx, m.Name, "203.0.113.10")
	got := ResolveMachineHost(ctx, m)
	if got != "203.0.113.10" {
		t.Fatalf("cache should win: got %q, want %q", got, "203.0.113.10")
	}
}

func TestResolveMachineHost_fallbackToSpec(t *testing.T) {
	ctx := scaffold.EnsurePlanCache(context.Background())
	m := manifestdata.Machine{Name: "static-1", Host: "static.example.com"}
	got := ResolveMachineHost(ctx, m)
	if got != "static.example.com" {
		t.Fatalf("fallback to Spec.Host: got %q", got)
	}
}

func TestResolveMachineHost_noCacheCtx(t *testing.T) {
	m := manifestdata.Machine{Name: "web", Host: "host.example.com"}
	got := ResolveMachineHost(context.Background(), m)
	if got != "host.example.com" {
		t.Fatalf("no plan-cache ctx: expected Spec.Host fallback, got %q", got)
	}
}

func TestResolveMachineHost_linodeMissingCache(t *testing.T) {
	// Linode machine with no manifest Host and no cache entry — returns
	// empty, which is the old behavior for the probe phase before
	// provisioning has resolved the instance.
	ctx := scaffold.EnsurePlanCache(context.Background())
	m := manifestdata.Machine{Name: "linode-gateway"}
	if got := ResolveMachineHost(ctx, m); got != "" {
		t.Fatalf("want empty host for unresolved Linode, got %q", got)
	}
}

// MachineBootstrapUser resolves the privileged bootstrap identity. It
// does NOT fall back to AgentUser — the two users are deliberately
// independent roles so hardening can disable the bootstrap account
// without breaking ongoing agent-user access.

func TestMachineBootstrapUser_defaultsToRoot(t *testing.T) {
	if got := MachineBootstrapUser(manifestdata.Machine{}); got != "root" {
		t.Fatalf("MachineBootstrapUser = %q, want root", got)
	}
}

func TestMachineBootstrapUser_explicit(t *testing.T) {
	m := manifestdata.Machine{BootstrapUser: "ubuntu"}
	if got := MachineBootstrapUser(m); got != "ubuntu" {
		t.Fatalf("MachineBootstrapUser = %q, want ubuntu", got)
	}
}

func TestMachineBootstrapUser_ignoresAgentUser(t *testing.T) {
	// Architectural invariant: AgentUser must never leak into the
	// bootstrap identity. A manifest with only agent_user set bootstraps
	// as root, not as the agent.
	m := manifestdata.Machine{AgentUser: "agent"}
	if got := MachineBootstrapUser(m); got != "root" {
		t.Fatalf("MachineBootstrapUser = %q, want root (agent_user must not leak)", got)
	}
}

// MachineAgentUser resolves the ongoing-operations identity used by
// every post-security phase. Falls back to root when unset for static
// ssh-type machines that don't declare an agent user.

func TestMachineAgentUser_defaultsToRoot(t *testing.T) {
	if got := MachineAgentUser(manifestdata.Machine{}); got != "root" {
		t.Fatalf("MachineAgentUser = %q, want root", got)
	}
}

func TestMachineAgentUser_explicit(t *testing.T) {
	m := manifestdata.Machine{AgentUser: "agent"}
	if got := MachineAgentUser(m); got != "agent" {
		t.Fatalf("MachineAgentUser = %q, want agent", got)
	}
}

func TestMachineAgentUser_ignoresBootstrapUser(t *testing.T) {
	// Architectural invariant: BootstrapUser must never leak into the
	// agent identity. A manifest with only bootstrap_user set runs app
	// phases as root, not as the bootstrap user.
	m := manifestdata.Machine{BootstrapUser: "ubuntu"}
	if got := MachineAgentUser(m); got != "root" {
		t.Fatalf("MachineAgentUser = %q, want root (bootstrap_user must not leak)", got)
	}
}
