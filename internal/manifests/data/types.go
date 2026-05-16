package data

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentsMDValue holds the value of an agent's agents_md field.
// It unmarshals from either a plain YAML string or a YAML sequence of strings.
// When a sequence is given, the entries are joined with a double newline so
// operators can compose a shared base section (via a YAML anchor) with
// agent-specific content without repeating the shared text:
//
//	x-team: &team |
//	  ## Team & Project
//	  ...
//
//	agents:
//	  - id: dev
//	    agents_md:
//	      - |
//	          You are the dev agent...
//	      - *team
type AgentsMDValue string

func (a *AgentsMDValue) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*a = AgentsMDValue(value.Value)
		return nil
	case yaml.SequenceNode:
		var parts []string
		if err := value.Decode(&parts); err != nil {
			return fmt.Errorf("agents_md: %w", err)
		}
		*a = AgentsMDValue(strings.Join(parts, "\n\n"))
		return nil
	default:
		return fmt.Errorf("agents_md: expected string or sequence of strings, got %s", value.Tag)
	}
}

// MachineType identifies how a machine is provisioned or reached.
type MachineType string

const (
	MachineTypeLinode    MachineType = "linode"
	MachineTypeSSH       MachineType = "ssh"
	MachineTypeMultipass MachineType = "multipass"
	// MachineTypeSelf is the synthetic machine type for the "self" reserved
	// target. Automation steps that list `machines: [self]` (or a step that
	// transfers files with self as one side) resolve to this type. The
	// automation runner short-circuits the SSH path for self targets and
	// dispatches locally via os/exec instead — there is no Host / SSHPort /
	// user associated with self. Gated by Manifest.AllowSelf to avoid
	// surprising operators who don't want remote manifests running on their
	// workstation.
	MachineTypeSelf MachineType = "self"
)

// SelfMachineName is the reserved target name that resolves to the machine
// running `claws`. It is intentionally not configurable: manifests referring
// to "self" always mean "this host, right now, in this process". Any real
// machine that happens to be named "self" in the `machines:` block is
// rejected by Validate.
const SelfMachineName = "self"

// IsHostedMachineType reports whether the machine type is backed by a
// cloud/VM provider (anything that isn't bare SSH). Hosted types go through
// the `provisioning` phase's create-machine/authorize-ssh-key/ensure-agent-
// user steps; SSH-typed machines are assumed pre-provisioned and skip them.
//
// Centralizing this keeps "what counts as hosted" in one place so adding a
// third provider later is a one-line change here, not a grep-and-replace
// across every Applicable() gate.
func IsHostedMachineType(t MachineType) bool {
	switch t {
	case MachineTypeLinode, MachineTypeMultipass:
		return true
	default:
		return false
	}
}

// Arch is a published OpenClaw node architecture label.
type Arch string

const (
	ArchLinuxX64    Arch = "linux-x64"
	ArchLinuxARM64  Arch = "linux-arm64"
	ArchDarwinX64   Arch = "darwin-x64"
	ArchDarwinARM64 Arch = "darwin-arm64"
)

// ChannelKind is a bot / channel integration type.
type ChannelKind string

const (
	ChannelKindTelegram ChannelKind = "telegram"
	ChannelKindSlack    ChannelKind = "slack"
	ChannelKindDiscord  ChannelKind = "discord"
)

// ManifestOptions holds optional behaviour toggles for the apply pipeline.
type ManifestOptions struct {
	// UnsetTMOUT adds an unset-tmout provisioning step that removes the TMOUT
	// environment variable from the bootstrap user's shell profiles. Useful for
	// hosting providers (e.g. OVH) that ship images with TMOUT set system-wide,
	// which causes SSH sessions to be killed mid-script with exit status 142
	// (SIGALRM). The step is idempotent and runs once during provisioning.
	UnsetTMOUT bool `yaml:"unset_tmout,omitempty"`
}

// Manifest is the top-level manifest document (YAML). SSH identity is stored
// separately (auth config / state), not embedded in the manifest, so the same
// manifest can be shared across users.
type Manifest struct {
	Prefix             string              `yaml:"prefix"`
	EnvFile            string              `yaml:"env_file"`
	NodeMajor          int                 `yaml:"node_major"`
	LinodeTokenEnv     string              `yaml:"linode_token_env"`
	Options            *ManifestOptions    `yaml:"options,omitempty"`
	Machines           []Machine           `yaml:"machines"`
	Gateways           []Gateway           `yaml:"gateways"`
	Agents             []Agent             `yaml:"agents"`
	Nodes              []Node              `yaml:"nodes"`
	Automations        []Automation        `yaml:"automations,omitempty"`

	// AllowSelf opts this manifest into running automation steps on the
	// operator's workstation (the machine running `claws`) via the reserved
	// "self" target. Without this flag, any automation referencing `self`
	// or any `scp.upload` / `scp.download` step is rejected at load/plan
	// time — we don't want a manifest pulled from a repo to suddenly exec
	// bash on a contributor's laptop just because they ran `claws apply`.
	AllowSelf bool `yaml:"allow_self,omitempty"`
}

// Automation is a user-defined phase with custom steps. Each automation
// becomes one scaffold phase targeting the listed machines.
// Non-manual automations are included in `claws apply`; manual ones
// run only via `claws automations apply`.
type Automation struct {
	Name        string   `yaml:"name"`
	Machines    []string `yaml:"machines"`
	Manual      bool     `yaml:"manual,omitempty"`
	Concurrency int      `yaml:"concurrency,omitempty"`
	RunAs       string   `yaml:"run_as,omitempty"`
	// Env is a phase-wide allowlist of environment-variable names that should
	// be exported into every lifecycle script for every step. Values are
	// resolved from the process env first, then the manifest's env_file (same
	// precedence as LookupEnvFromManifest). Per-step Env entries extend this
	// list. Default empty: nothing is leaked into scripts unless explicitly
	// listed.
	Env   []string         `yaml:"env,omitempty"`
	Steps []AutomationStep `yaml:"steps"`
}

// Supported AutomationStep.Kind values. Empty string == StepKindBash.
const (
	StepKindBash         = "bash"
	StepKindPython       = "python"
	StepKindSCPUpload    = "scp.upload"
	StepKindSCPDownload  = "scp.download"
)

// AutomationStep is one unit of work inside an automation phase.
// Lifecycle scripts map directly to scaffold step methods.
//
// `kind` selects the dispatcher:
//   - "" / "bash" / "python" — lifecycle scripts run on each target machine
//     (or locally when the target is "self"). Source/Destination/Mode are
//     ignored.
//   - "scp.upload" / "scp.download" — Execute transfers files between the
//     operator (self) and the target machine via SFTP. Applicable / Check /
//     Verify still run as bash scripts on the remote side (for idempotency
//     probes). Execute / ExecuteFile are not used and should be left empty.
type AutomationStep struct {
	Name  string `yaml:"name"`
	Kind  string `yaml:"kind,omitempty"`
	RunAs string `yaml:"run_as,omitempty"`
	// Env is a per-step allowlist of env-var names, unioned with the parent
	// Automation.Env. See Automation.Env for precedence rules.
	Env            []string `yaml:"env,omitempty"`
	Applicable     string   `yaml:"applicable,omitempty"`
	ApplicableFile string   `yaml:"applicable_file,omitempty"`
	Check          string   `yaml:"check,omitempty"`
	CheckFile      string   `yaml:"check_file,omitempty"`
	Execute        string   `yaml:"execute,omitempty"`
	ExecuteFile    string   `yaml:"execute_file,omitempty"`
	Verify         string   `yaml:"verify,omitempty"`
	VerifyFile     string   `yaml:"verify_file,omitempty"`

	// Source / Destination / Mode are only consumed by scp.upload /
	// scp.download steps.
	//
	// For scp.upload:
	//   - Source      is a path on the operator host (self).
	//   - Destination is a path on the remote target machine.
	// For scp.download:
	//   - Source      is a path on the remote target machine.
	//   - Destination is a path on the operator host (self).
	//
	// Mode is an optional octal permission string ("0755", "0600") applied
	// to the destination side after the transfer completes. When empty,
	// the destination inherits the source's permissions for files and 0755
	// for newly-created directories.
	//
	// Whether the transfer is a single file or a recursive directory copy
	// is auto-detected from the source side: directories recurse, regular
	// files stream. Symlinks are followed (resolved to their target).
	Source      string `yaml:"source,omitempty"`
	Destination string `yaml:"destination,omitempty"`
	Mode        string `yaml:"mode,omitempty"`

	// IfChanged, when true on a scp.upload step, turns the step into a
	// content-addressed upload: the default Check compares the SHA256 of
	// the local source to the SHA256 of the remote destination (via
	// `sha256sum` on the target). When they match the step is satisfied
	// and Execute is skipped; when they differ (or the remote file is
	// absent) Execute runs the SFTP transfer.
	//
	// Only valid for kind: scp.upload. An explicit `check:` / `check_file:`
	// on the same step takes precedence over this default — if the
	// operator wrote their own idempotency probe, we respect it. Regular
	// files only for now; directory sources with if_changed are rejected
	// at validation time (recursive hashing is a bigger change).
	IfChanged bool `yaml:"if_changed,omitempty"`
}

// Machine is a host entry (cloud or static SSH).
//
// The CPUs/Memory/Disk fields only apply to multipass-typed machines —
// Linode uses SKU for the same concept, and SSH-typed entries ignore both.
// Keeping all of them on one struct avoids a provider-typed union at the
// YAML layer at the cost of a few fields being valid only for certain
// types; callers gate on Type before reading.
type Machine struct {
	Name         string      `yaml:"name"`
	Type         MachineType `yaml:"type"`
	SKU          string      `yaml:"sku"`
	Image        string      `yaml:"image"`
	Region       string      `yaml:"region"`
	Host         string      `yaml:"host"`
	InternalHost string      `yaml:"internal_host,omitempty"`
	SSHPort      int         `yaml:"ssh_port,omitempty"`
	// BootstrapUser is the privileged identity used ONLY during the
	// provisioning and security phases to bring the machine up and lock it
	// down. It is never used post-bootstrap; every app-level phase (mesh,
	// node, gateway, agents, channels, automations) dials the machine as
	// AgentUser instead. See common.MachineBootstrapUser /
	// common.MachineAgentUser for the resolved-value helpers.
	BootstrapUser string `yaml:"bootstrap_user"`
	AgentUser     string `yaml:"agent_user"`
	SSHKeyEnv     string `yaml:"ssh_key_env"`
	Arch          Arch   `yaml:"arch"`
	Container     bool   `yaml:"container,omitempty"`
	// Multipass-only sizing. Strings match the `multipass launch` CLI
	// units ("1G", "512M", "5G", ...). CPUs=0 / empty Memory / empty Disk
	// let the provider pick sensible defaults.
	CPUs   int    `yaml:"cpus,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	Disk   string `yaml:"disk,omitempty"`
}

// Gateway is an OpenClaw gateway instance bound to a machine reference.
type Gateway struct {
	Name            string             `yaml:"name"`
	Reference       string             `yaml:"reference"`
	OpenclawVersion string             `yaml:"openclaw_version"`
	Networking      *GatewayNetworking `yaml:"networking"`
	Channels        []Channel          `yaml:"channels"`
}

// GatewayNetworking configures mesh / public exposure for a gateway.
type GatewayNetworking struct {
	Mode             string              `yaml:"mode"`
	PublicHostname   *PublicHostnameSpec `yaml:"public_hostname"`
	PreauthKeyEnv    string              `yaml:"preauth_key_env"`
	PreauthKeyFile   string              `yaml:"preauth_key_file"`
	PreauthKeySource string              `yaml:"preauth_key_source"`
}

// PublicHostnameSpec controls how the public HTTPS hostname is derived.
type PublicHostnameSpec struct {
	Strategy string `yaml:"strategy"`
	Host     string `yaml:"host"`
}

// Channel is a bot account wired to a gateway.
type Channel struct {
	Kind     ChannelKind `yaml:"kind"`
	Name     string      `yaml:"name"`
	TokenEnv string      `yaml:"token_env"`
	Target   string      `yaml:"target"`
	Default  bool        `yaml:"default"`
}

// Node is a remote execution node paired with a gateway.
type Node struct {
	Name            string          `yaml:"name"`
	Gateway         string          `yaml:"gateway"`
	Reference       string          `yaml:"reference"`
	OpenclawVersion string          `yaml:"openclaw_version,omitempty"`
	ExecPolicy      *NodeExecPolicy `yaml:"exec_policy,omitempty"`
}

// NodeExecPolicy constrains exec and approval behavior on a node.
type NodeExecPolicy struct {
	Security string `yaml:"security,omitempty"`
	Ask      string `yaml:"ask,omitempty"`
}

// Agent is an OpenClaw agent profile.
type Agent struct {
	ID        string         `yaml:"id"`
	Gateway   string         `yaml:"gateway"`
	Workspace string         `yaml:"workspace"`
	Model     AgentModel     `yaml:"model"`
	Tools     *AgentTools    `yaml:"tools,omitempty"`
	Soul      string         `yaml:"soul,omitempty"`
	AgentsMD  AgentsMDValue  `yaml:"agents_md,omitempty"`
	Identity  *AgentIdentity `yaml:"identity,omitempty"`
	Bindings  []AgentBinding `yaml:"bindings,omitempty"`
}

// AgentModel selects the model and optional fallbacks.
type AgentModel struct {
	Primary   string   `yaml:"primary"`
	Fallbacks []string `yaml:"fallbacks,omitempty"`
}

// AgentTools configures tool access for an agent.
type AgentTools struct {
	Exec     *AgentExec     `yaml:"exec,omitempty"`
	Elevated *AgentElevated `yaml:"elevated,omitempty"`
}

// AgentExec pins exec to a gateway host or node.
type AgentExec struct {
	Host     string `yaml:"host"`
	Node     string `yaml:"node,omitempty"`
	Security string `yaml:"security,omitempty"`
}

// AgentElevated configures elevated tool usage and Telegram allowlists.
type AgentElevated struct {
	Enabled   *bool               `yaml:"enabled,omitempty"`
	AllowFrom map[string][]string `yaml:"allow_from,omitempty"`
}

// AgentIdentity is display metadata for an agent.
type AgentIdentity struct {
	Name  string `yaml:"name,omitempty"`
	Emoji string `yaml:"emoji,omitempty"`
}

// AgentBinding ties an agent to a channel account.
type AgentBinding struct {
	Channel string `yaml:"channel"`
	Account string `yaml:"account,omitempty"`
}
