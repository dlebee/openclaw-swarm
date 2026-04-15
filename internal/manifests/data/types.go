package data

// MachineType identifies how a machine is provisioned or reached.
type MachineType string

const (
	MachineTypeLinode MachineType = "linode"
	MachineTypeSSH    MachineType = "ssh"
)

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

// Manifest is the top-level manifest document (YAML). SSH identity is stored
// separately (auth config / state), not embedded in the manifest, so the same
// manifest can be shared across users.
type Manifest struct {
	Prefix             string              `yaml:"prefix"`
	EnvFile            string              `yaml:"env_file"`
	NodeMajor          int                 `yaml:"node_major"`
	LinodeTokenEnv     string              `yaml:"linode_token_env"`
	Machines           []Machine           `yaml:"machines"`
	KubernetesClusters []KubernetesCluster `yaml:"kubernetes-clusters"`
	Gateways           []Gateway           `yaml:"gateways"`
	Agents             []Agent             `yaml:"agents"`
	Nodes              []Node              `yaml:"nodes"`
	Automations        []Automation        `yaml:"automations,omitempty"`
}

// Automation runs custom probe/execute scripts on selected machines.
type Automation struct {
	Name        string   `yaml:"name"`
	Machines    []string `yaml:"machines"`
	Kind        string   `yaml:"kind,omitempty"`
	RunAs       string   `yaml:"run_as,omitempty"`
	Manual      bool     `yaml:"manual,omitempty"`
	Probe       string   `yaml:"probe,omitempty"`
	Execute     string   `yaml:"execute,omitempty"`
	Clean       string   `yaml:"clean,omitempty"`
	ProbeFile   string   `yaml:"probe_file,omitempty"`
	ExecuteFile string   `yaml:"execute_file,omitempty"`
	CleanFile   string   `yaml:"clean_file,omitempty"`
}

// Machine is a host entry (cloud or static SSH).
type Machine struct {
	Name         string      `yaml:"name"`
	Type         MachineType `yaml:"type"`
	SKU          string      `yaml:"sku"`
	Image        string      `yaml:"image"`
	Region       string      `yaml:"region"`
	Host         string      `yaml:"host"`
	InternalHost string      `yaml:"internal_host,omitempty"`
	SSHPort      int         `yaml:"ssh_port,omitempty"`
	SSHUser      string      `yaml:"ssh_user"`
	AgentUser    string      `yaml:"agent_user"`
	SSHKeyEnv    string      `yaml:"ssh_key_env"`
	Arch         Arch        `yaml:"arch"`
}

// KubernetesCluster references a kubeconfig for optional K8s automation.
type KubernetesCluster struct {
	Name          string `yaml:"name"`
	Kubeconfig    string `yaml:"kubeconfig"`
	KubeconfigEnv string `yaml:"kubeconfig_env"`
	ClusterName   string `yaml:"cluster_name"`
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
	Name       string          `yaml:"name"`
	Gateway    string          `yaml:"gateway"`
	Reference  string          `yaml:"reference"`
	ExecPolicy *NodeExecPolicy `yaml:"exec_policy,omitempty"`
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
	AgentsMD  string         `yaml:"agents_md,omitempty"`
	Identity  *AgentIdentity `yaml:"identity,omitempty"`
	Bootstrap string         `yaml:"bootstrap,omitempty"`
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
