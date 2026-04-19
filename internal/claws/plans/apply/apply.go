// Package apply builds and runs the manifest infrastructure apply scaffold plan.
// Phases live under apply/<phase> (e.g. apply/provisioning).
package apply

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/agents"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/channels"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/mesh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/node"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/security"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/automations"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

// BuildOptions is everything needed to assemble the apply plan (phases in order).
type BuildOptions struct {
	Manifest     *manifestdata.Manifest
	ManifestPath string // absolute path to the manifest file (for env_file resolution)
	Provider     hosting.Provider
	SSHPubKey    string
	// SSHDial is required when the manifest has any Linode machine (authorize-ssh-key step).
	SSHDial provisioning.SSHDialFunc
	// IncludeManualAutomations also injects automations with manual=true into
	// the pipeline. By default only non-manual ones are included.
	IncludeManualAutomations bool
}

// BuildPlan returns a scaffold plan: phases are appended in apply order (provisioning first).
func BuildPlan(o BuildOptions) (*scaffold.Plan, error) {
	if o.Manifest == nil {
		return nil, fmt.Errorf("apply plan: manifest is nil")
	}
	if hasHostedMachines(o.Manifest.Machines) && o.SSHDial == nil {
		return nil, fmt.Errorf("apply plan: SSHDial is required when the manifest has hosted (linode/multipass) machines")
	}
	targets := provisioning.BuildMachineTargets(o.Manifest.Machines)

	p := scaffold.New()
	provisioning.AddPhase(p, targets, provisioning.Options{
		Provider:  o.Provider,
		Prefix:    o.Manifest.Prefix,
		SSHPubKey: o.SSHPubKey,
		SSHDial:   o.SSHDial,
	})
	security.AddPhase(p, targets, security.Options{
		SSHDial: security.SSHDialFunc(o.SSHDial),
	})
	if len(o.Manifest.Automations) > 0 {
		resolvedEnv, err := manifestsvc.LoadEnvFile(o.ManifestPath, o.Manifest)
		if err != nil {
			return nil, fmt.Errorf("apply plan: load env_file for automations: %w", err)
		}
		autoOpts := automations.Options{
			SSHDial:             automations.SSHDialFunc(o.SSHDial),
			ManifestDir:         manifestDir(o.ManifestPath),
			MachineTargets:      targets,
			ResolvedEnv:         resolvedEnv,
			AssumeWillProvision: true,
		}
		automations.AddPhases(p, o.Manifest.Automations, autoOpts, false)
		if o.IncludeManualAutomations {
			automations.AddPhases(p, o.Manifest.Automations, autoOpts, true)
		}
	}
	mesh.AddPhase(p, targets, mesh.Options{
		SSHDial:  mesh.SSHDialFunc(o.SSHDial),
		Machines: o.Manifest.Machines,
		Gateways: o.Manifest.Gateways,
		Nodes:    o.Manifest.Nodes,
	})
	if len(o.Manifest.Gateways) > 0 {
		gwTargets := gateway.BuildGatewayTargets(o.Manifest.Gateways, o.Manifest.Machines)
		gateway.AddPhase(p, gwTargets, gateway.Options{
			SSHDial: gateway.SSHDialFunc(o.SSHDial),
		})
	}
	if hasChannels(o.Manifest.Gateways) {
		chTargets, err := channels.BuildChannelTargets(
			o.Manifest.Gateways, o.Manifest.Machines,
			func(envName string) (string, error) {
				return manifestsvc.LookupEnvFromManifest(o.ManifestPath, o.Manifest, envName)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("apply plan: resolve channel tokens: %w", err)
		}
		if len(chTargets) > 0 {
			channels.AddPhase(p, chTargets, channels.Options{
				SSHDial: channels.SSHDialFunc(o.SSHDial),
			})
		}
	}
	if len(o.Manifest.Nodes) > 0 {
		nodeTargets := node.BuildNodeTargets(o.Manifest.Nodes, o.Manifest.Machines, o.Manifest.Gateways)
		node.AddPhase(p, nodeTargets, node.Options{
			SSHDial: node.SSHDialFunc(o.SSHDial),
		})
	}
	if len(o.Manifest.Agents) > 0 {
		agentTargets := agents.BuildAgentTargets(o.Manifest.Agents, o.Manifest.Gateways, o.Manifest.Machines)
		agents.AddPhase(p, agentTargets, agents.Options{
			SSHDial: agents.SSHDialFunc(o.SSHDial),
		})
	}

	// PreRun installs a host resolver that every non-provisioning phase
	// consults via common.ResolveMachineHost when the per-machine host
	// isn't in the plan cache yet. On a hot run (full apply pipeline)
	// provisioning.create-machine writes the entries first and this
	// resolver is never hit. On a cold `--only <phase>` run — or any
	// run where an earlier phase was skipped — the resolver lazily
	// re-hydrates from the provider, so phases don't need to couple
	// their correctness to an earlier phase's in-memory state. One-shot
	// guarded by a plan-cache flag so a run doesn't re-ListByTag for
	// every machine lookup.
	if o.Provider != nil {
		provider := o.Provider
		prefix := o.Manifest.Prefix
		machines := o.Manifest.Machines
		p.PreRun = func(ctx context.Context) error {
			common.RegisterHostResolver(ctx, func(ctx context.Context, machineName string) (string, bool, error) {
				if h, ok := scaffold.LookupPlanMachineHost(ctx, machineName); ok && h != "" {
					return h, true, nil
				}
				if _, done := scaffold.PlanCacheGet(ctx, "APPLY_HOST_RESOLVER_DONE"); !done {
					targets := provisioning.BuildMachineTargets(machines)
					if err := provisioning.ResolveHostedInstances(ctx, provider, prefix, targets); err != nil {
						return "", false, err
					}
					scaffold.PlanCacheSet(ctx, "APPLY_HOST_RESOLVER_DONE", true)
				}
				if h, ok := scaffold.LookupPlanMachineHost(ctx, machineName); ok && h != "" {
					return h, true, nil
				}
				return "", false, nil
			})
			return nil
		}
	}
	return p, nil
}

// ProviderFromManifest returns the hosting.Provider that matches the
// manifest's non-SSH machine type. Contract:
//
//   - No hosted machines (all type: ssh) → returns (nil, nil). Apply runs
//     but the provisioning phase is a no-op for every target.
//   - All hosted machines are Linode → returns a Linode provider, requires
//     linode_token_env to be set and resolve.
//   - All hosted machines are Multipass → returns a Multipass provider.
//     No token needed.
//   - Manifest mixes hosted types (e.g. Linode + Multipass in the same
//     run) → error. Apply per-run is single-provider by design; if you
//     want mixed hosting use two separate manifests or provision
//     externally and use type: ssh for the second fleet.
//
// manifestAbsPath is the absolute path to the manifest file, used to
// resolve relative env_file entries for the Linode token.
func ProviderFromManifest(m *manifestdata.Manifest, manifestAbsPath string) (hosting.Provider, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	kinds := hostedKinds(m.Machines)
	switch len(kinds) {
	case 0:
		return nil, nil
	case 1:
		// fall through with the single kind
	default:
		return nil, fmt.Errorf("apply plan: machines mix hosted types %v in one manifest; split into separate manifests or use type: ssh", kinds)
	}
	kind := kinds[0]
	switch kind {
	case manifestdata.MachineTypeLinode:
		if strings.TrimSpace(m.LinodeTokenEnv) == "" {
			return nil, fmt.Errorf("manifest linode_token_env is required when machines use type %q", manifestdata.MachineTypeLinode)
		}
		tok, err := manifestsvc.LookupEnvFromManifest(manifestAbsPath, m, m.LinodeTokenEnv)
		if err != nil {
			return nil, err
		}
		return linode.NewProvider(tok), nil
	case manifestdata.MachineTypeMultipass:
		return multipass.NewProvider(multipass.Options{})
	default:
		return nil, fmt.Errorf("apply plan: no provider for machine type %q", kind)
	}
}

// LinodeProviderFromManifest is a deprecated alias retained for older call
// sites that still expect the Linode-only API. New callers must use
// ProviderFromManifest.
//
// Deprecated: use ProviderFromManifest.
func LinodeProviderFromManifest(m *manifestdata.Manifest, manifestAbsPath string) (hosting.Provider, error) {
	return ProviderFromManifest(m, manifestAbsPath)
}

// RunOptions configures ExecWithConfirm for an apply run.
//
// OnlyPhases and SkipPhases let callers run a subset of the apply pipeline —
// used by integration tests ("only run provisioning + security") and by
// operators iterating on a single phase. Both filters apply to the prepared
// plan preview AND execution, so the tree always reflects what will run.
// Unknown phase names are silently ignored at this layer; callers should
// validate against scaffold.Plan.PhaseNames before reaching Run.
type RunOptions struct {
	DryRun      bool
	Out         io.Writer
	ProgressOut io.Writer
	Confirm     func() (bool, error)
	// PrettyPlan shows the plan in a Bubble Tea viewport (alternate screen) when Out is a TTY.
	PrettyPlan bool
	OnlyPhases []string
	SkipPhases []string
}

// Run builds progress styling from ProgressOut, resolves width from Out when possible, then ExecWithConfirm.
func Run(ctx context.Context, plan *scaffold.Plan, o RunOptions) error {
	width := 80
	if f, ok := o.Out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w >= 40 {
			width = w
		}
	}
	progOut := o.ProgressOut
	if progOut == nil {
		progOut = io.Discard
	}
	styled := progress.NewStyled(progOut)

	var teaWriter io.Writer
	if f, ok := progOut.(*os.File); ok && !o.DryRun {
		if term.IsTerminal(int(f.Fd())) {
			teaWriter = f
		}
	}

	return scaffold.ExecWithConfirm(ctx, plan, scaffold.PipelineOptions{
		ExecuteOptions: scaffold.ExecuteOptions{
			DryRun:     o.DryRun,
			Progress:   styled,
			OnlyPhases: o.OnlyPhases,
			SkipPhases: o.SkipPhases,
		},
		BuildProgress:     styled,
		Confirm:           o.Confirm,
		Width:             width,
		Out:               o.Out,
		PrettyPlan:        o.PrettyPlan,
		TeaProgressWriter: teaWriter,
	})
}

func manifestDir(manifestPath string) string {
	if manifestPath == "" {
		return ""
	}
	return filepath.Dir(manifestPath)
}

func hasHostedMachines(machines []manifestdata.Machine) bool {
	for _, m := range machines {
		if manifestdata.IsHostedMachineType(m.Type) {
			return true
		}
	}
	return false
}

// hostedKinds returns the deduplicated set of hosted machine types in the
// manifest (everything that's not SSH). Order is insertion order so
// single-kind manifests round-trip deterministically.
func hostedKinds(machines []manifestdata.Machine) []manifestdata.MachineType {
	seen := make(map[manifestdata.MachineType]struct{})
	var out []manifestdata.MachineType
	for _, m := range machines {
		if !manifestdata.IsHostedMachineType(m.Type) {
			continue
		}
		if _, ok := seen[m.Type]; ok {
			continue
		}
		seen[m.Type] = struct{}{}
		out = append(out, m.Type)
	}
	return out
}

func hasChannels(gateways []manifestdata.Gateway) bool {
	for _, gw := range gateways {
		if len(gw.Channels) > 0 {
			return true
		}
	}
	return false
}
