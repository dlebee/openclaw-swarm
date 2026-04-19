package automations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/python"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// DynamicStep wraps an AutomationStep definition and implements scaffold.Step.
// The lifecycle scripts (applicable, check, execute, verify) are dispatched
// over SSH using bash or python depending on the step's Kind.
type DynamicStep struct {
	step        manifestdata.AutomationStep
	defaultUser string   // automation-level run_as
	autoEnv     []string // automation-level env allowlist
	opts        Options
}

// NewDynamicStep creates a DynamicStep from an AutomationStep definition.
// defaultRunAs/autoEnv are the phase-level defaults (Automation.RunAs /
// Automation.Env) that each step inherits unless it overrides them.
func NewDynamicStep(step manifestdata.AutomationStep, defaultRunAs string, autoEnv []string, opts Options) *DynamicStep {
	return &DynamicStep{
		step:        step,
		defaultUser: defaultRunAs,
		autoEnv:     autoEnv,
		opts:        opts,
	}
}

func (d *DynamicStep) Name() string { return d.step.Name }

// Applicable decides whether the step should run for this target.
//
// When the target machine has no reachable host (e.g. a Linode without an
// Instance yet), behavior depends on Options.AssumeWillProvision:
//   - AssumeWillProvision=true  (inside `claws apply`): assume the machine
//     will be provisioned later in the plan — return true. The real dial/check
//     happens at execute time.
//   - AssumeWillProvision=false (standalone `claws automations apply`): the
//     machine simply doesn't exist — return false to skip.
//
// Otherwise runs the applicable script if set; not set = always true.
func (d *DynamicStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	if at, ok := t.Payload.(*AutomationTarget); ok && at.Host() == "" {
		return d.opts.AssumeWillProvision, nil
	}
	script, err := d.resolveScript(d.step.Applicable, d.step.ApplicableFile)
	if err != nil {
		return false, fmt.Errorf("%s: resolve applicable: %w", d.step.Name, err)
	}
	if script == "" {
		return true, nil
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return false, fmt.Errorf("%s: applicable dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	err = d.run(client, script)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// Check runs the check script if set; not set = always false (always execute).
//
// When the target has no reachable host and AssumeWillProvision=true (plan
// probe inside `claws apply`), return false so the step is shown as "will
// execute" — the real check runs once provisioning populates the Instance.
func (d *DynamicStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	if at, ok := t.Payload.(*AutomationTarget); ok && at.Host() == "" {
		return false, nil
	}
	script, err := d.resolveScript(d.step.Check, d.step.CheckFile)
	if err != nil {
		return false, fmt.Errorf("%s: resolve check: %w", d.step.Name, err)
	}
	if script == "" {
		return false, nil
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return false, fmt.Errorf("%s: check dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	err = d.run(client, script)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// Execute runs the execute script. Required.
func (d *DynamicStep) Execute(ctx context.Context, t scaffold.Target) error {
	script, err := d.resolveScript(d.step.Execute, d.step.ExecuteFile)
	if err != nil {
		return fmt.Errorf("%s: resolve execute: %w", d.step.Name, err)
	}
	if script == "" {
		return fmt.Errorf("%s: execute script is required", d.step.Name)
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return fmt.Errorf("%s: execute dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	if err := d.run(client, script); err != nil {
		return fmt.Errorf("%s: %w", d.step.Name, err)
	}
	return nil
}

// Verify runs the verify script if set; falls back to the check script
// when no explicit verify is provided; noop when neither is set.
func (d *DynamicStep) Verify(ctx context.Context, t scaffold.Target) error {
	script, err := d.resolveScript(d.step.Verify, d.step.VerifyFile)
	if err != nil {
		return fmt.Errorf("%s: resolve verify: %w", d.step.Name, err)
	}
	if script == "" {
		script, err = d.resolveScript(d.step.Check, d.step.CheckFile)
		if err != nil {
			return fmt.Errorf("%s: resolve verify (check fallback): %w", d.step.Name, err)
		}
	}
	if script == "" {
		return nil
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return fmt.Errorf("%s: verify dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	if err := d.run(client, script); err != nil {
		return fmt.Errorf("%s: verify: %w", d.step.Name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (d *DynamicStep) resolveScript(inline, file string) (string, error) {
	if s := strings.TrimSpace(inline); s != "" {
		return s, nil
	}
	if file == "" {
		return "", nil
	}
	path := file
	if !filepath.IsAbs(path) && d.opts.ManifestDir != "" {
		path = filepath.Join(d.opts.ManifestDir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	return string(b), nil
}

// runAs resolves the SSH user for a dynamic automation step. Automations
// run after the apply plan is fully converged, so the default identity is
// the agent user (never the bootstrap user, which may have been disabled
// by hardening). Step- and automation-level overrides let power users
// escape the default when they need to (e.g. a one-off root maintenance
// script), but those overrides stay opt-in.
//
//  1. Step-level run_as override (most specific)
//  2. Automation-level run_as override
//  3. Machine-level agent_user (the normal case)
//  4. "root" (only when the manifest opted out of an agent user)
func (d *DynamicStep) runAs(m manifestdata.Machine) string {
	if u := strings.TrimSpace(d.step.RunAs); u != "" {
		return u
	}
	if u := strings.TrimSpace(d.defaultUser); u != "" {
		return u
	}
	return common.MachineAgentUser(m)
}

func (d *DynamicStep) kind() string {
	if k := strings.TrimSpace(d.step.Kind); k != "" {
		return strings.ToLower(k)
	}
	return "bash"
}

func (d *DynamicStep) dial(ctx context.Context, t scaffold.Target) (*xssh.Client, string, error) {
	at, ok := t.Payload.(*AutomationTarget)
	if !ok {
		return nil, "", fmt.Errorf("unexpected target payload type %T", t.Payload)
	}
	m := at.Machine()
	host := at.Host()
	if host == "" {
		return nil, "", fmt.Errorf("machine %q has no reachable host yet (not provisioned?)", m.Name)
	}
	port := common.MachineSSHPort(m)
	user := d.runAs(m)
	return common.BorrowSSH(ctx, d.opts.SSHDial, host, port, user)
}

func (d *DynamicStep) run(client *xssh.Client, script string) error {
	kind := d.kind()
	envNames := d.effectiveEnvAllowlist()
	envValues := resolveEnvValues(envNames, d.opts.ResolvedEnv)
	switch kind {
	case "python":
		return python.Run(client, pythonEnvPreamble(envValues)+script)
	default:
		return bash.Run(client, bashEnvPreamble(envValues)+script)
	}
}

// effectiveEnvAllowlist returns the union of the automation-level and
// step-level `env:` lists, preserving declaration order and removing dups.
// Listing the same variable at both levels is fine and idempotent — the
// step list simply reaffirms it.
func (d *DynamicStep) effectiveEnvAllowlist() []string {
	if len(d.autoEnv) == 0 && len(d.step.Env) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(d.autoEnv)+len(d.step.Env))
	out := make([]string, 0, len(d.autoEnv)+len(d.step.Env))
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, n := range d.autoEnv {
		add(n)
	}
	for _, n := range d.step.Env {
		add(n)
	}
	return out
}
