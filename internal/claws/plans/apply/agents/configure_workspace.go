package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

const (
	managedStart = "<!-- CLAWS MANAGED START - Do not edit this section. It is overwritten by `claws apply`. -->"
	managedEnd   = "<!-- CLAWS MANAGED END -->"
)

// ConfigureWorkspaceStep writes/updates workspace files (SOUL.md, IDENTITY.md,
// AGENTS.md) using managed section markers to preserve user content, and sets
// identity config via `openclaw agents set-identity`.
type ConfigureWorkspaceStep struct {
	dial SSHDialFunc
}

func NewConfigureWorkspaceStep(opts Options) *ConfigureWorkspaceStep {
	return &ConfigureWorkspaceStep{dial: opts.SSHDial}
}

func (*ConfigureWorkspaceStep) Name() string { return "configure-workspace" }

func (s *ConfigureWorkspaceStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*AgentTarget)
	return ok, nil
}

// managedFile describes a workspace file whose content lives inside markers.
type managedFile struct {
	name    string
	content string
}

func desiredFiles(spec manifestdata.Agent) []managedFile {
	var files []managedFile
	if spec.Soul != "" {
		files = append(files, managedFile{name: "SOUL.md", content: spec.Soul})
	}
	if spec.AgentsMD != "" {
		files = append(files, managedFile{name: "AGENTS.md", content: spec.AgentsMD})
	}
	if spec.Identity != nil && spec.Identity.Name != "" {
		files = append(files, managedFile{name: "IDENTITY.md", content: buildIdentityMD(spec.Identity)})
	}
	return files
}

func buildIdentityMD(id *manifestdata.AgentIdentity) string {
	var sb strings.Builder
	sb.WriteString("# IDENTITY.md - Agent Identity\n\n")
	if id.Name != "" {
		sb.WriteString(fmt.Sprintf("name: %s\n", id.Name))
	}
	if id.Emoji != "" {
		sb.WriteString(fmt.Sprintf("emoji: %s\n", id.Emoji))
	}
	return sb.String()
}

// resolveWorkspace resolves ~ in the workspace path using the remote $HOME.
func resolveWorkspace(client *xssh.Client, workspace string) (string, error) {
	if strings.HasPrefix(workspace, "~/") {
		home, err := gwService.ResolveHome(client)
		if err != nil {
			return "", err
		}
		return path.Join(home, workspace[2:]), nil
	}
	return workspace, nil
}

func (s *ConfigureWorkspaceStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	wsDir, err := resolveWorkspace(client, at.Spec.Workspace)
	if err != nil {
		return false, fmt.Errorf("resolve workspace on %s: %w", m.Name, err)
	}

	for _, f := range desiredFiles(at.Spec) {
		matches, err := managedSectionMatchesE(client, path.Join(wsDir, f.name), f.content)
		if err != nil {
			return false, fmt.Errorf("check %s on %s: %w", f.name, m.Name, err)
		}
		if !matches {
			return false, nil
		}
	}
	return true, nil
}

func (s *ConfigureWorkspaceStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-workspace: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	wsDir, err := resolveWorkspace(client, at.Spec.Workspace)
	if err != nil {
		return fmt.Errorf("configure-workspace: %w", err)
	}

	for _, f := range desiredFiles(at.Spec) {
		if err := writeManagedSection(client, path.Join(wsDir, f.name), f.content); err != nil {
			return fmt.Errorf("configure-workspace: %s: %w", f.name, err)
		}
	}

	if at.Spec.Identity != nil && at.Spec.Identity.Name != "" {
		cmd := fmt.Sprintf(`openclaw agents set-identity --agent %q --name %q`, at.Spec.ID, at.Spec.Identity.Name)
		if at.Spec.Identity.Emoji != "" {
			cmd += fmt.Sprintf(` --emoji %q`, at.Spec.Identity.Emoji)
		}
		out, err := bash.RunOutput(client, cmd)
		if err != nil {
			return fmt.Errorf("configure-workspace: set-identity: %w\n%s", err, out)
		}
	}

	return nil
}

func (s *ConfigureWorkspaceStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-workspace verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	wsDir, err := resolveWorkspace(client, at.Spec.Workspace)
	if err != nil {
		return fmt.Errorf("configure-workspace verify: %w", err)
	}

	for _, f := range desiredFiles(at.Spec) {
		if !managedSectionMatches(client, path.Join(wsDir, f.name), f.content) {
			return fmt.Errorf("configure-workspace verify: %s managed section mismatch", f.name)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// managed section helpers
// ---------------------------------------------------------------------------

// wrapManaged wraps content in CLAWS MANAGED markers with a separator for the
// user's own content.
func wrapManaged(content string) string {
	return managedStart + "\n\n" + strings.TrimSpace(content) + "\n\n" + managedEnd + "\n\n---\nEverything below is yours. Add, edit, evolve freely.\n"
}

// extractManagedSection returns the content between the CLAWS MANAGED markers,
// or "" if markers are absent.
func extractManagedSection(fileContent string) string {
	startIdx := strings.Index(fileContent, managedStart)
	if startIdx < 0 {
		return ""
	}
	after := fileContent[startIdx+len(managedStart):]
	endIdx := strings.Index(after, managedEnd)
	if endIdx < 0 {
		return ""
	}
	return strings.TrimSpace(after[:endIdx])
}

// managedSectionMatches reads a remote file and checks whether its managed
// section content matches the desired content. Kept for callers (Verify) that
// legitimately treat absence-or-drift as a single failure mode; new callers
// should prefer managedSectionMatchesE so real SFTP errors propagate.
func managedSectionMatches(client *xssh.Client, filePath, desired string) bool {
	matches, _ := managedSectionMatchesE(client, filePath, desired)
	return matches
}

// managedSectionMatchesE is the error-aware variant: absence is (false, nil);
// any other SFTP/read error is returned verbatim so Check can surface it as
// "check: ..." in the probe UI instead of silently reporting "will execute".
func managedSectionMatchesE(client *xssh.Client, filePath, desired string) (bool, error) {
	data, err := sshfile.ReadFile(client, filePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return extractManagedSection(string(data)) == strings.TrimSpace(desired), nil
}

// writeManagedSection creates or updates a file's managed section, preserving
// any user content outside the markers.
func writeManagedSection(client *xssh.Client, filePath, content string) error {
	content = strings.TrimSpace(content)
	managedBlock := managedStart + "\n\n" + content + "\n\n" + managedEnd

	existing, err := sshfile.ReadFile(client, filePath)
	if errors.Is(err, os.ErrNotExist) {
		return sshfile.WriteFile(client, filePath, []byte(wrapManaged(content)))
	}
	if err != nil {
		return err
	}

	text := string(existing)
	startIdx := strings.Index(text, managedStart)
	endIdx := strings.Index(text, managedEnd)
	if startIdx >= 0 && endIdx > startIdx {
		// Replace managed section, keep everything before and after.
		before := text[:startIdx]
		after := text[endIdx+len(managedEnd):]
		return sshfile.WriteFile(client, filePath, []byte(before+managedBlock+after))
	}

	// No markers found — inject at the top, preserve existing content.
	return sshfile.WriteFile(client, filePath, []byte(managedBlock+"\n\n"+text))
}

