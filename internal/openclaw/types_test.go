package openclaw

import (
	"encoding/json"
	"testing"
)

func TestNodeListNode_Unmarshal(t *testing.T) {
	raw := `{
		"nodeId": "abc-123",
		"displayName": "my-node",
		"platform": "linux",
		"coreVersion": "1.2.3",
		"remoteIp": "100.64.0.5",
		"caps": ["exec", "camera"],
		"permissions": {"admin": true, "write": false},
		"paired": true,
		"connected": true,
		"connectedAtMs": 1700000000000
	}`
	var n NodeListNode
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.NodeID != "abc-123" {
		t.Errorf("nodeId: got %q", n.NodeID)
	}
	if n.DisplayName != "my-node" {
		t.Errorf("displayName: got %q", n.DisplayName)
	}
	if len(n.Caps) != 2 || n.Caps[0] != "exec" {
		t.Errorf("caps: got %v", n.Caps)
	}
	if !n.Permissions["admin"] || n.Permissions["write"] {
		t.Errorf("permissions: got %v", n.Permissions)
	}
	if !n.Connected {
		t.Error("expected connected=true")
	}
	if n.ConnectedAtMs == nil || *n.ConnectedAtMs != 1700000000000 {
		t.Errorf("connectedAtMs: got %v", n.ConnectedAtMs)
	}
}

func TestNodesStatusResult_Unmarshal(t *testing.T) {
	raw := `{
		"ts": 1700000000000,
		"nodes": [
			{"nodeId": "n1", "connected": true},
			{"nodeId": "n2", "connected": false}
		]
	}`
	var r NodesStatusResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Ts != 1700000000000 {
		t.Errorf("ts: got %d", r.Ts)
	}
	if len(r.Nodes) != 2 {
		t.Fatalf("nodes: got %d", len(r.Nodes))
	}
	if !r.Nodes[0].Connected {
		t.Error("nodes[0] should be connected")
	}
}

func TestPairingList_Unmarshal(t *testing.T) {
	raw := `{
		"pending": [
			{
				"requestId": "req-1",
				"nodeId": "n1",
				"ts": 1700000000000,
				"requiredApproveScopes": ["operator.pairing"]
			}
		],
		"paired": [
			{
				"nodeId": "n2",
				"displayName": "paired-node",
				"permissions": {"admin": true},
				"lastConnectedAtMs": 1700000099000
			}
		]
	}`
	var pl PairingList
	if err := json.Unmarshal([]byte(raw), &pl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(pl.Pending) != 1 {
		t.Fatalf("pending: got %d", len(pl.Pending))
	}
	if pl.Pending[0].RequestID != "req-1" {
		t.Errorf("requestId: got %q", pl.Pending[0].RequestID)
	}
	if len(pl.Pending[0].RequiredApproveScopes) != 1 {
		t.Errorf("scopes: got %v", pl.Pending[0].RequiredApproveScopes)
	}
	if len(pl.Paired) != 1 {
		t.Fatalf("paired: got %d", len(pl.Paired))
	}
	if pl.Paired[0].DisplayName != "paired-node" {
		t.Errorf("displayName: got %q", pl.Paired[0].DisplayName)
	}
	if pl.Paired[0].LastConnectedAtMs == nil || *pl.Paired[0].LastConnectedAtMs != 1700000099000 {
		t.Errorf("lastConnectedAtMs: got %v", pl.Paired[0].LastConnectedAtMs)
	}
}

func TestPairedNode_OmitsEmptyFields(t *testing.T) {
	n := PairedNode{NodeID: "x"}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	if _, ok := m["token"]; ok {
		t.Error("empty token should be omitted")
	}
	if _, ok := m["lastConnectedAtMs"]; ok {
		t.Error("nil lastConnectedAtMs should be omitted")
	}
}

func TestDevicePairingList_Unmarshal(t *testing.T) {
	raw := `{
		"pending": [
			{
				"requestId": "dreq-1",
				"deviceId": "dev-001",
				"displayName": "macbook",
				"roles": ["operator"],
				"scopes": ["device.control"],
				"remoteIp": "192.168.1.10",
				"ts": 1700000000000
			}
		],
		"paired": [
			{
				"deviceId": "dev-002",
				"displayName": "iphone",
				"platform": "ios",
				"roles": ["user"],
				"tokens": [
					{"role": "user", "createdAtMs": 1700000000000, "lastUsedAtMs": 1700000099000}
				],
				"createdAtMs": 1700000000000,
				"approvedAtMs": 1700000001000
			}
		]
	}`
	var dl DevicePairingList
	if err := json.Unmarshal([]byte(raw), &dl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dl.Pending) != 1 {
		t.Fatalf("pending: got %d", len(dl.Pending))
	}
	if dl.Pending[0].DeviceID != "dev-001" {
		t.Errorf("deviceId: got %q", dl.Pending[0].DeviceID)
	}
	if len(dl.Pending[0].Roles) != 1 || dl.Pending[0].Roles[0] != "operator" {
		t.Errorf("roles: got %v", dl.Pending[0].Roles)
	}
	if len(dl.Paired) != 1 {
		t.Fatalf("paired: got %d", len(dl.Paired))
	}
	if dl.Paired[0].DisplayName != "iphone" {
		t.Errorf("displayName: got %q", dl.Paired[0].DisplayName)
	}
	if len(dl.Paired[0].Tokens) != 1 || dl.Paired[0].Tokens[0].Role != "user" {
		t.Errorf("tokens: got %v", dl.Paired[0].Tokens)
	}
}

func TestChannelsListResult_Unmarshal(t *testing.T) {
	raw := `{
		"chat": {
			"discord": ["acc-1", "acc-2"],
			"telegram": ["acc-3"]
		},
		"auth": [
			{"id": "prof-1", "provider": "openai", "type": "api_key", "isExternal": false}
		]
	}`
	var cl ChannelsListResult
	if err := json.Unmarshal([]byte(raw), &cl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cl.Chat) != 2 {
		t.Fatalf("chat: got %d channels", len(cl.Chat))
	}
	if len(cl.Chat["discord"]) != 2 {
		t.Errorf("discord accounts: got %v", cl.Chat["discord"])
	}
	if len(cl.Auth) != 1 || cl.Auth[0].Provider != "openai" {
		t.Errorf("auth: got %v", cl.Auth)
	}
}

func TestAgentSummary_Unmarshal(t *testing.T) {
	raw := `[
		{
			"id": "agent-1",
			"name": "default",
			"workspace": "/home/user/.openclaw/agents/default",
			"agentDir": "default",
			"model": "gpt-4",
			"bindings": 2,
			"isDefault": true,
			"routes": ["discord/acc-1"],
			"providers": ["openai"]
		}
	]`
	var agents []AgentSummary
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents: got %d", len(agents))
	}
	a := agents[0]
	if a.ID != "agent-1" {
		t.Errorf("id: got %q", a.ID)
	}
	if a.Model != "gpt-4" {
		t.Errorf("model: got %q", a.Model)
	}
	if a.Bindings != 2 {
		t.Errorf("bindings: got %d", a.Bindings)
	}
	if !a.IsDefault {
		t.Error("expected isDefault=true")
	}
	if len(a.Routes) != 1 || a.Routes[0] != "discord/acc-1" {
		t.Errorf("routes: got %v", a.Routes)
	}
}

func TestAgentBinding_Unmarshal(t *testing.T) {
	raw := `[
		{
			"agentId": "agent-1",
			"match": {
				"channel": "discord",
				"accountId": "acc-1",
				"peer": {"kind": "direct", "id": "user-123"},
				"guildId": "guild-1"
			},
			"description": "discord/acc-1 direct user-123"
		}
	]`
	var bindings []AgentBinding
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings: got %d", len(bindings))
	}
	b := bindings[0]
	if b.AgentID != "agent-1" {
		t.Errorf("agentId: got %q", b.AgentID)
	}
	if b.Match.Channel != "discord" {
		t.Errorf("channel: got %q", b.Match.Channel)
	}
	if b.Match.Peer == nil || b.Match.Peer.Kind != "direct" {
		t.Errorf("peer: got %v", b.Match.Peer)
	}
	if b.Match.GuildID != "guild-1" {
		t.Errorf("guildId: got %q", b.Match.GuildID)
	}
	if b.Description != "discord/acc-1 direct user-123" {
		t.Errorf("description: got %q", b.Description)
	}
}

func TestHostString(t *testing.T) {
	h := Host{Addr: "10.0.0.1", Port: 22, User: "root"}
	if got := h.String(); got != "root@10.0.0.1:22" {
		t.Errorf("got %q", got)
	}
}
