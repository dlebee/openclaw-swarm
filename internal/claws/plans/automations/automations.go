// Package automations builds scaffold phases from manifest automation definitions.
// Each Automation becomes one phase; its AutomationSteps become scaffold Steps.
package automations

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc = common.SSHDialFunc

// AutomationTarget is the scaffold target payload for automation phases.
// It carries a pointer to the shared provisioning.MachineTarget so Linode
// machines can resolve their dynamic host (Instance.PublicIPv4) the same
// way the mesh/security phases do.
type AutomationTarget struct {
	MachineTarget *provisioning.MachineTarget
}

// Machine returns the manifest machine spec.
func (at *AutomationTarget) Machine() manifestdata.Machine {
	if at.MachineTarget == nil {
		return manifestdata.Machine{}
	}
	return at.MachineTarget.Spec
}

// Host resolves the reachable host. Prefers Instance.PublicIPv4 (Linode after
// provisioning) over the static Spec.Host.
func (at *AutomationTarget) Host() string {
	if at.MachineTarget == nil {
		return ""
	}
	if at.MachineTarget.Instance != nil {
		if h := strings.TrimSpace(at.MachineTarget.Instance.PublicIPv4); h != "" {
			return h
		}
	}
	return strings.TrimSpace(at.MachineTarget.Spec.Host)
}

// GetMachine implements common.MachineProvider.
func (at *AutomationTarget) GetMachine() manifestdata.Machine { return at.Machine() }

// IsSelf reports whether this target is the reserved "self" target —
// i.e. the automation step should run on the operator host rather than
// being dialed over SSH. Detection is by MachineType so manifests can't
// accidentally create a non-self target that happens to be named "self"
// (validation rejects that anyway, but this is the source of truth).
func (at *AutomationTarget) IsSelf() bool {
	if at == nil || at.MachineTarget == nil {
		return false
	}
	return at.MachineTarget.Spec.Type == manifestdata.MachineTypeSelf
}

// Options configures AddPhases / BuildPlan.
type Options struct {
	SSHDial        SSHDialFunc
	ManifestDir    string
	MachineTargets []scaffold.Target // shared provisioning targets (for dynamic host resolution)

	// ResolvedEnv is the merged process+env_file variable map that
	// automation steps can opt into exposing via their `env:` allowlist.
	// Populated once at plan-build time (see BuildPlan). When empty, steps
	// with an `env:` list log a warning per missing variable but don't fail
	// — the script will just see an undefined var.
	ResolvedEnv map[string]string

	// AssumeWillProvision affects behavior when a target's host is not yet
	// resolvable (e.g. Linode without an Instance during plan probe inside
	// `claws apply`):
	//   - true  : Applicable=true, Check=false (shows as "will execute" in the
	//             plan; the real dial/check happens at execute time once
	//             provisioning has populated the Instance).
	//   - false : Applicable=false (step is skipped — appropriate for
	//             `claws automations apply` where provisioning isn't in the
	//             plan and the machine simply doesn't exist).
	AssumeWillProvision bool
}

// AddPhases appends one scaffold phase per automation to plan. Pass manual=true
// to include only manual automations (claws automations apply), or manual=false
// to include only non-manual ones (injected into claws apply).
func AddPhases(p *scaffold.Plan, automations []manifestdata.Automation, opts Options, manual bool) {
	mtByName := indexMachineTargets(opts.MachineTargets)
	for _, auto := range automations {
		if auto.Manual != manual {
			continue
		}
		addAutomationPhase(p, auto, mtByName, opts)
	}
}

// AddPhasesFiltered is like AddPhases but only includes automations whose
// names appear in the filter set. Manual flag is ignored when filtering by name.
func AddPhasesFiltered(p *scaffold.Plan, automations []manifestdata.Automation, opts Options, names []string) {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	mtByName := indexMachineTargets(opts.MachineTargets)
	for _, auto := range automations {
		if !nameSet[auto.Name] {
			continue
		}
		addAutomationPhase(p, auto, mtByName, opts)
	}
}

func addAutomationPhase(p *scaffold.Plan, auto manifestdata.Automation, mtByName map[string]*provisioning.MachineTarget, opts Options) {
	targets := buildTargets(auto, mtByName)
	if len(targets) == 0 || len(auto.Steps) == 0 {
		return
	}
	ph := p.AddPhase(auto.Name)
	ph.Concurrency = effectiveConcurrency(auto)
	ph.AddTargets(targets...)
	for _, step := range auto.Steps {
		ph.AddStep(NewDynamicStep(step, auto.RunAs, auto.Env, opts))
	}
}

func buildTargets(auto manifestdata.Automation, mtByName map[string]*provisioning.MachineTarget) []scaffold.Target {
	targets := make([]scaffold.Target, 0, len(auto.Machines))
	for _, name := range auto.Machines {
		if manifestdata.IsSelfMachineName(name) {
			// "self" is a synthetic target — no manifest machine entry,
			// no SSH dial. We still synthesize a MachineTarget so the
			// rest of the plumbing (AutomationTarget, IsSelf probes) can
			// stay untouched; consumers check MachineTypeSelf to branch.
			targets = append(targets, scaffold.Target{
				ID: manifestdata.SelfMachineName,
				Payload: &AutomationTarget{MachineTarget: &provisioning.MachineTarget{
					Spec: manifestdata.Machine{
						Name: manifestdata.SelfMachineName,
						Type: manifestdata.MachineTypeSelf,
					},
				}},
			})
			continue
		}
		mt, ok := mtByName[name]
		if !ok {
			continue
		}
		targets = append(targets, scaffold.Target{
			ID:      name,
			Payload: &AutomationTarget{MachineTarget: mt},
		})
	}
	return targets
}

func effectiveConcurrency(auto manifestdata.Automation) int {
	if auto.Concurrency > 0 {
		return auto.Concurrency
	}
	return 1
}

// indexMachineTargets builds a name->*MachineTarget index from shared
// provisioning targets. Non-MachineTarget payloads are ignored.
func indexMachineTargets(targets []scaffold.Target) map[string]*provisioning.MachineTarget {
	out := make(map[string]*provisioning.MachineTarget, len(targets))
	for _, t := range targets {
		if mt, ok := t.Payload.(*provisioning.MachineTarget); ok && mt != nil {
			out[t.ID] = mt
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Standalone pipeline (claws automations apply)
// ---------------------------------------------------------------------------

// BuildPlan creates a scaffold plan from all automations (or a filtered subset).
// Opts.MachineTargets may be empty; when empty, one is constructed from machines
// so non-Linode machines (with static Spec.Host) still work.
func BuildPlan(automations []manifestdata.Automation, machines []manifestdata.Machine, opts Options, names []string) *scaffold.Plan {
	if len(opts.MachineTargets) == 0 {
		opts.MachineTargets = provisioning.BuildMachineTargets(machines)
	}
	p := scaffold.New()
	if len(names) > 0 {
		AddPhasesFiltered(p, automations, opts, names)
	} else {
		mtByName := indexMachineTargets(opts.MachineTargets)
		for _, auto := range automations {
			addAutomationPhase(p, auto, mtByName, opts)
		}
	}
	return p
}

// RunOptions configures the standalone automations pipeline.
type RunOptions struct {
	DryRun      bool
	Out         io.Writer
	ProgressOut io.Writer
	Confirm     func() (bool, error)
	PrettyPlan  bool

	// Force, when true, runs every applicable step's Execute even if its
	// Check script would have reported "satisfied". Handy for re-running
	// one-shot maintenance automations where the check can't distinguish
	// "already done" from "needs doing again". See
	// scaffold.ExecuteOptions.Force for the detailed semantics.
	Force bool
}

// Run executes the automations plan through scaffold.ExecWithConfirm.
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
			DryRun:   o.DryRun,
			Force:    o.Force,
			Progress: styled,
		},
		BuildProgress:     styled,
		Confirm:           o.Confirm,
		Width:             width,
		Out:               o.Out,
		PrettyPlan:        o.PrettyPlan,
		TeaProgressWriter: teaWriter,
	})
}
