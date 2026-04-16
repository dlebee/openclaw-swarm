package openclaw

// ---------------------------------------------------------------------------
// openclaw nodes status --json
// ---------------------------------------------------------------------------

// NodeListNode is one node from `openclaw nodes status --json`.
type NodeListNode struct {
	NodeID          string          `json:"nodeId"`
	DisplayName     string          `json:"displayName,omitempty"`
	Platform        string          `json:"platform,omitempty"`
	Version         string          `json:"version,omitempty"`
	CoreVersion     string          `json:"coreVersion,omitempty"`
	UIVersion       string          `json:"uiVersion,omitempty"`
	ClientID        string          `json:"clientId,omitempty"`
	ClientMode      string          `json:"clientMode,omitempty"`
	RemoteIP        string          `json:"remoteIp,omitempty"`
	DeviceFamily    string          `json:"deviceFamily,omitempty"`
	ModelIdentifier string          `json:"modelIdentifier,omitempty"`
	PathEnv         string          `json:"pathEnv,omitempty"`
	Caps            []string        `json:"caps,omitempty"`
	Commands        []string        `json:"commands,omitempty"`
	Permissions     map[string]bool `json:"permissions,omitempty"`
	Paired          bool            `json:"paired,omitempty"`
	Connected       bool            `json:"connected,omitempty"`
	ConnectedAtMs   *int64          `json:"connectedAtMs,omitempty"`
	ApprovedAtMs    *int64          `json:"approvedAtMs,omitempty"`
}

// NodesStatusResult is the top-level JSON from `openclaw nodes status --json`.
type NodesStatusResult struct {
	Ts    int64          `json:"ts"`
	Nodes []NodeListNode `json:"nodes"`
}

// ---------------------------------------------------------------------------
// openclaw nodes list --json
// ---------------------------------------------------------------------------

// PendingRequest is a node awaiting gateway approval.
type PendingRequest struct {
	RequestID             string   `json:"requestId"`
	NodeID                string   `json:"nodeId"`
	DisplayName           string   `json:"displayName,omitempty"`
	Platform              string   `json:"platform,omitempty"`
	Version               string   `json:"version,omitempty"`
	CoreVersion           string   `json:"coreVersion,omitempty"`
	UIVersion             string   `json:"uiVersion,omitempty"`
	RemoteIP              string   `json:"remoteIp,omitempty"`
	Ts                    int64    `json:"ts"`
	Commands              []string `json:"commands,omitempty"`
	RequiredApproveScopes []string `json:"requiredApproveScopes,omitempty"`
}

// PairedNode is a node that has been approved and paired with the gateway.
type PairedNode struct {
	NodeID            string          `json:"nodeId"`
	Token             string          `json:"token,omitempty"`
	DisplayName       string          `json:"displayName,omitempty"`
	Platform          string          `json:"platform,omitempty"`
	Version           string          `json:"version,omitempty"`
	CoreVersion       string          `json:"coreVersion,omitempty"`
	UIVersion         string          `json:"uiVersion,omitempty"`
	RemoteIP          string          `json:"remoteIp,omitempty"`
	Permissions       map[string]bool `json:"permissions,omitempty"`
	CreatedAtMs       *int64          `json:"createdAtMs,omitempty"`
	ApprovedAtMs      *int64          `json:"approvedAtMs,omitempty"`
	LastConnectedAtMs *int64          `json:"lastConnectedAtMs,omitempty"`
}

// PairingList is the JSON from `openclaw nodes list --json`.
type PairingList struct {
	Pending []PendingRequest `json:"pending"`
	Paired  []PairedNode     `json:"paired"`
}

// ---------------------------------------------------------------------------
// openclaw devices list --json
// ---------------------------------------------------------------------------

// DeviceTokenSummary is a redacted token summary within a PairedDevice.
type DeviceTokenSummary struct {
	Role         string `json:"role"`
	Scopes       []string `json:"scopes,omitempty"`
	CreatedAtMs  *int64 `json:"createdAtMs,omitempty"`
	RotatedAtMs  *int64 `json:"rotatedAtMs,omitempty"`
	RevokedAtMs  *int64 `json:"revokedAtMs,omitempty"`
	LastUsedAtMs *int64 `json:"lastUsedAtMs,omitempty"`
}

// PendingDevice is a device awaiting gateway approval.
type PendingDevice struct {
	RequestID    string   `json:"requestId"`
	DeviceID     string   `json:"deviceId"`
	PublicKey    string   `json:"publicKey,omitempty"`
	DisplayName  string   `json:"displayName,omitempty"`
	Platform     string   `json:"platform,omitempty"`
	DeviceFamily string   `json:"deviceFamily,omitempty"`
	ClientID     string   `json:"clientId,omitempty"`
	ClientMode   string   `json:"clientMode,omitempty"`
	Role         string   `json:"role,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	RemoteIP     string   `json:"remoteIp,omitempty"`
	Silent       bool     `json:"silent,omitempty"`
	IsRepair     bool     `json:"isRepair,omitempty"`
	Ts           int64    `json:"ts"`
}

// PairedDevice is a device that has been approved and paired.
type PairedDevice struct {
	DeviceID     string               `json:"deviceId"`
	DisplayName  string               `json:"displayName,omitempty"`
	Platform     string               `json:"platform,omitempty"`
	DeviceFamily string               `json:"deviceFamily,omitempty"`
	ClientID     string               `json:"clientId,omitempty"`
	ClientMode   string               `json:"clientMode,omitempty"`
	Roles        []string             `json:"roles,omitempty"`
	Scopes       []string             `json:"scopes,omitempty"`
	RemoteIP     string               `json:"remoteIp,omitempty"`
	Tokens       []DeviceTokenSummary `json:"tokens,omitempty"`
	CreatedAtMs  *int64               `json:"createdAtMs,omitempty"`
	ApprovedAtMs *int64               `json:"approvedAtMs,omitempty"`
}

// DevicePairingList is the JSON from `openclaw devices list --json`.
type DevicePairingList struct {
	Pending []PendingDevice `json:"pending"`
	Paired  []PairedDevice  `json:"paired"`
}

// ---------------------------------------------------------------------------
// openclaw channels list --json
// ---------------------------------------------------------------------------

// AuthProfile describes one auth profile entry in the channels list response.
type AuthProfile struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Type       string `json:"type"`
	IsExternal bool   `json:"isExternal"`
}

// ChannelsListResult is the JSON from `openclaw channels list --json`.
type ChannelsListResult struct {
	Chat map[string][]string `json:"chat"`
	Auth []AuthProfile       `json:"auth"`
}

// ---------------------------------------------------------------------------
// openclaw agents list --json  (top-level is a JSON array)
// ---------------------------------------------------------------------------

// AgentSummary is one element from `openclaw agents list --json`.
type AgentSummary struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	IdentityName   string   `json:"identityName,omitempty"`
	IdentityEmoji  string   `json:"identityEmoji,omitempty"`
	IdentitySource string   `json:"identitySource,omitempty"`
	Workspace      string   `json:"workspace"`
	AgentDir       string   `json:"agentDir"`
	Model          string   `json:"model,omitempty"`
	Bindings       int      `json:"bindings"`
	BindingDetails []string `json:"bindingDetails,omitempty"`
	Routes         []string `json:"routes,omitempty"`
	Providers      []string `json:"providers,omitempty"`
	IsDefault      bool     `json:"isDefault"`
}

// ---------------------------------------------------------------------------
// openclaw agents bindings --agent <id> --json  (top-level is a JSON array)
// ---------------------------------------------------------------------------

// BindingPeer identifies a specific chat target in a binding match.
type BindingPeer struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// AgentBindingMatch specifies which channel/account/peer a binding applies to.
type AgentBindingMatch struct {
	Channel   string       `json:"channel"`
	AccountID string       `json:"accountId,omitempty"`
	Peer      *BindingPeer `json:"peer,omitempty"`
	GuildID   string       `json:"guildId,omitempty"`
	TeamID    string       `json:"teamId,omitempty"`
	Roles     []string     `json:"roles,omitempty"`
}

// AgentBinding is one routing rule from `openclaw agents bindings --json`.
type AgentBinding struct {
	AgentID     string            `json:"agentId"`
	Match       AgentBindingMatch `json:"match"`
	Description string            `json:"description"`
}
