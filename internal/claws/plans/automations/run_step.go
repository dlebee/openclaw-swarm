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
	defaultUser string // automation-level run_as
	opts        Options
}

// NewDynamicStep creates a DynamicStep from an AutomationStep definition.
func NewDynamicStep(step manifestdata.AutomationStep, defaultRunAs string, opts Options) *DynamicStep {
	return &DynamicStep{
		step:        step,
		defaultUser: defaultRunAs,
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

func (d *DynamicStep) runAs() string {
	if u := strings.TrimSpace(d.step.RunAs); u != "" {
		return u
	}
	if u := strings.TrimSpace(d.defaultUser); u != "" {
		return u
	}
	return "root"
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
	user := d.runAs()
	return common.BorrowSSH(ctx, d.opts.SSHDial, host, port, user)
}

func (d *DynamicStep) run(client *xssh.Client, script string) error {
	switch d.kind() {
	case "python":
		return python.Run(client, script)
	default:
		return bash.Run(client, script)
	}
}
