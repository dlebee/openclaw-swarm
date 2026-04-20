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
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// DynamicStep wraps an AutomationStep definition and implements scaffold.Step.
// The lifecycle scripts (applicable, check, execute, verify) are dispatched
// via bash or python — over SSH for remote targets, via os/exec for the
// reserved "self" target. scp.upload / scp.download kinds do not run an
// Execute script: Execute performs a streaming SFTP transfer between the
// operator host and the remote target instead.
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
// Self targets bypass the host-empty short-circuit entirely: there's
// nothing to provision, so an empty "host" is the normal state.
//
// Otherwise runs the applicable script if set; not set = always true.
func (d *DynamicStep) Applicable(ctx context.Context, t scaffold.Target) (bool, error) {
	if at, ok := t.Payload.(*AutomationTarget); ok && !at.IsSelf() && at.Host() == "" {
		return d.opts.AssumeWillProvision, nil
	}
	script, err := d.resolveScript(d.step.Applicable, d.step.ApplicableFile)
	if err != nil {
		return false, fmt.Errorf("%s: resolve applicable: %w", d.step.Name, err)
	}
	if script == "" {
		return true, nil
	}
	runErr, dialErr := d.probeScript(ctx, t, script)
	if dialErr != nil {
		return false, fmt.Errorf("%s: applicable dial: %w", d.step.Name, dialErr)
	}
	if runErr != nil {
		return false, nil
	}
	return true, nil
}

// Check reports whether the step is already satisfied.
//
// Precedence:
//  1. Explicit `check:` / `check_file:` script on the step — run it,
//     exit-0 ⇒ satisfied.
//  2. kind: scp.upload + if_changed: true and no explicit check — run
//     the content-hash default: satisfied iff sha256(local source) ==
//     sha256sum(remote destination).
//  3. Otherwise not satisfied (Execute will run every time).
//
// When a remote target has no reachable host and AssumeWillProvision=true
// (plan probe inside `claws apply`), return false so the step is shown as
// "will execute" — the real check runs once provisioning populates the
// Instance. Self targets skip this short-circuit (no provisioning needed).
func (d *DynamicStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	if at, ok := t.Payload.(*AutomationTarget); ok && !at.IsSelf() && at.Host() == "" {
		return false, nil
	}
	script, err := d.resolveScript(d.step.Check, d.step.CheckFile)
	if err != nil {
		return false, fmt.Errorf("%s: resolve check: %w", d.step.Name, err)
	}
	if script == "" {
		if d.kind() == manifestdata.StepKindSCPUpload && d.step.IfChanged {
			return d.checkUploadByHash(ctx, t)
		}
		return false, nil
	}
	runErr, dialErr := d.probeScript(ctx, t, script)
	if dialErr != nil {
		return false, fmt.Errorf("%s: check dial: %w", d.step.Name, dialErr)
	}
	if runErr != nil {
		return false, nil
	}
	return true, nil
}

// checkUploadByHash implements the default idempotency probe for
// kind: scp.upload + if_changed: true. It compares the SHA256 of the
// local source file with the remote destination's sha256sum.
//
// Semantics:
//   - Remote file missing → not satisfied (let Execute upload it).
//   - Remote sha != local sha → not satisfied.
//   - Remote sha == local sha → satisfied, skip Execute.
//   - Any error (local hash read, SSH dial, remote sha256sum failure) is
//     surfaced as a Check error per the cache-purity rule "errors are not
//     cache misses" — the probe UI will render "check: <detail>" rather
//     than silently report "will execute".
//
// Directories are intentionally unsupported for if_changed (the
// validator / LocalSHA256 both reject them); the feature is scoped to
// single-file uploads where the hash equivalence is unambiguous.
func (d *DynamicStep) checkUploadByHash(ctx context.Context, t scaffold.Target) (bool, error) {
	localPath := d.resolveSourcePath()
	localSum, err := sshfile.LocalSHA256(localPath)
	if err != nil {
		return false, fmt.Errorf("%s: hash local %s: %w", d.step.Name, localPath, err)
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return false, fmt.Errorf("%s: check dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	remoteSum, exists, err := sshfile.RemoteSHA256(client, d.step.Destination)
	if err != nil {
		return false, fmt.Errorf("%s: hash remote %s: %w", d.step.Name, d.step.Destination, err)
	}
	if !exists {
		return false, nil
	}
	return remoteSum == localSum, nil
}

// resolveSourcePath resolves step.Source for scp.upload. Absolute paths
// are used as-is; relative paths are resolved against opts.ManifestDir
// (same contract as resolveScript for *_file fields). Without this the
// operator has to think about whether their cwd matches the manifest
// directory, which quietly bites during CI.
func (d *DynamicStep) resolveSourcePath() string {
	p := d.step.Source
	if filepath.IsAbs(p) || d.opts.ManifestDir == "" {
		return p
	}
	return filepath.Join(d.opts.ManifestDir, p)
}

// Execute runs the step's main work:
//   - bash / python: execute the `execute` (or execute_file) script on the
//     target (remotely over SSH, or locally when the target is self).
//   - scp.upload: stream step.Source (operator host) -> step.Destination
//     (remote target) via SFTP.
//   - scp.download: stream step.Source (remote target) -> step.Destination
//     (operator host) via SFTP.
func (d *DynamicStep) Execute(ctx context.Context, t scaffold.Target) error {
	switch d.kind() {
	case manifestdata.StepKindSCPUpload:
		return d.executeUpload(ctx, t)
	case manifestdata.StepKindSCPDownload:
		return d.executeDownload(ctx, t)
	}

	script, err := d.resolveScript(d.step.Execute, d.step.ExecuteFile)
	if err != nil {
		return fmt.Errorf("%s: resolve execute: %w", d.step.Name, err)
	}
	if script == "" {
		return fmt.Errorf("%s: execute script is required", d.step.Name)
	}
	if err := d.runScript(ctx, t, script); err != nil {
		return fmt.Errorf("%s: %w", d.step.Name, err)
	}
	return nil
}

// Verify runs the verify script if set; falls back to the check script
// when no explicit verify is provided; noop when neither is set.
//
// For scp.* kinds the verify/check scripts run on the *remote* side
// (the side we pushed to / pulled from) — not on self. That matches what
// you usually want to assert ("the file landed and has the right size /
// mode / checksum").
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
	if err := d.runScript(ctx, t, script); err != nil {
		return fmt.Errorf("%s: verify: %w", d.step.Name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// SCP dispatch
// ---------------------------------------------------------------------------

// executeUpload implements kind=scp.upload. Source is on the operator
// host (self), destination is on the remote machine. The manifest
// validator guarantees step.Source/Destination are non-empty and the
// target is NOT self — so we can go straight to dial + Upload without
// re-checking invariants that were already enforced at load time.
func (d *DynamicStep) executeUpload(ctx context.Context, t scaffold.Target) error {
	if isSelfTarget(t) {
		return fmt.Errorf("%s: scp.upload cannot target self (self is always the source side)", d.step.Name)
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return fmt.Errorf("%s: upload dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	mode := d.parsedMode()
	src := d.resolveSourcePath()
	if err := sshfile.Upload(client, src, d.step.Destination, mode); err != nil {
		return fmt.Errorf("%s: upload %s -> %s: %w", d.step.Name, src, d.step.Destination, err)
	}
	return nil
}

// executeDownload implements kind=scp.download. Source is on the remote
// machine, destination is on the operator host (self).
func (d *DynamicStep) executeDownload(ctx context.Context, t scaffold.Target) error {
	if isSelfTarget(t) {
		return fmt.Errorf("%s: scp.download cannot target self (self is always the destination side)", d.step.Name)
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return fmt.Errorf("%s: download dial: %w", d.step.Name, err)
	}
	defer common.ReturnSSH(ctx, key, client)

	mode := d.parsedMode()
	if err := sshfile.Download(client, d.step.Source, d.step.Destination, mode); err != nil {
		return fmt.Errorf("%s: download %s -> %s: %w", d.step.Name, d.step.Source, d.step.Destination, err)
	}
	return nil
}

// parsedMode converts the manifest's optional string mode into the
// *os.FileMode that sshfile.Upload/Download expect. Returns nil when the
// operator didn't specify one, which lets sshfile fall back to source
// permissions.
func (d *DynamicStep) parsedMode() *os.FileMode {
	v, ok := manifestdata.ParseMode(d.step.Mode)
	if !ok {
		return nil
	}
	m := os.FileMode(v)
	return &m
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
	return manifestdata.StepKindBash
}

// scriptKind returns the interpreter kind (bash / python) to use for
// lifecycle scripts. scp.* steps still run their applicable/check/verify
// as bash because that's the overwhelmingly common shape for idempotency
// probes ("does the file exist with the right checksum?"). Operators who
// want a python probe on an scp.* step can set RunAs at the step level —
// but that's not a door we open today to keep the mental model small.
func (d *DynamicStep) scriptKind() string {
	k := d.kind()
	if k == manifestdata.StepKindPython {
		return manifestdata.StepKindPython
	}
	return manifestdata.StepKindBash
}

func (d *DynamicStep) dial(ctx context.Context, t scaffold.Target) (*xssh.Client, string, error) {
	at, ok := t.Payload.(*AutomationTarget)
	if !ok {
		return nil, "", fmt.Errorf("unexpected target payload type %T", t.Payload)
	}
	m := at.Machine()
	host := at.Host()
	if host == "" {
		// Cold `claws apply --only <automation>`: MachineTarget.Instance is
		// often nil even though VMs exist — the same case as mesh/gateway
		// steps that call common.ResolveMachineHost (see apply.BuildPlan PreRun).
		host = common.ResolveMachineHost(ctx, m)
	}
	if host == "" {
		return nil, "", fmt.Errorf("machine %q has no reachable host yet (not provisioned?)", m.Name)
	}
	port := common.MachineSSHPort(m)
	user := d.runAs(m)
	return common.BorrowSSH(ctx, d.opts.SSHDial, host, port, user)
}

// runScript executes script against t, choosing the transport based on
// whether t is the reserved self target. Used by Execute/Verify where
// any failure (transport or script) is a step failure.
func (d *DynamicStep) runScript(ctx context.Context, t scaffold.Target, script string) error {
	runErr, dialErr := d.probeScript(ctx, t, script)
	if dialErr != nil {
		return dialErr
	}
	return runErr
}

// probeScript is the shared core for Applicable/Check/Execute/Verify. It
// returns the script error (separate from transport errors) so Applicable
// and Check can treat a non-zero script exit as "skip this step" while
// still propagating genuine dial failures as plan errors — matching the
// pre-self behavior of this package. For self targets there's no
// transport to fail independently, so dialErr is always nil and any
// runLocal failure lands in runErr.
func (d *DynamicStep) probeScript(ctx context.Context, t scaffold.Target, script string) (runErr, dialErr error) {
	envValues := resolveEnvValues(d.effectiveEnvAllowlist(), d.opts.ResolvedEnv)

	kind := d.scriptKind()
	var prefixed string
	if kind == manifestdata.StepKindPython {
		prefixed = pythonEnvPreamble(envValues) + script
	} else {
		prefixed = bashEnvPreamble(envValues) + script
	}

	if isSelfTarget(t) {
		// Pin cwd to the manifest dir so scripts using relative paths
		// (e.g. `cd model-orchestrator`) resolve deterministically
		// regardless of where the operator invoked claws from.
		return runLocal(ctx, kind, prefixed, runLocalOptions{
			workingDir: d.opts.ManifestDir,
		}), nil
	}
	client, key, err := d.dial(ctx, t)
	if err != nil {
		return nil, err
	}
	defer common.ReturnSSH(ctx, key, client)

	if kind == manifestdata.StepKindPython {
		return python.Run(client, prefixed), nil
	}
	return bash.Run(client, prefixed), nil
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

// isSelfTarget returns true when the target is the reserved self target.
// Accepts anything — non-AutomationTarget payloads are never self.
func isSelfTarget(t scaffold.Target) bool {
	at, ok := t.Payload.(*AutomationTarget)
	if !ok {
		return false
	}
	return at.IsSelf()
}
