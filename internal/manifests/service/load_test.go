package service

import (
	"path/filepath"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

func TestLoadDavidArmyExample(t *testing.T) {
	path := filepath.Join("testdata", "david-army.yml")
	m, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if m.Prefix != "dlebee" {
		t.Errorf("prefix: got %q want %q", m.Prefix, "dlebee")
	}
	if m.EnvFile != ".env" {
		t.Errorf("env_file: got %q want %q", m.EnvFile, ".env")
	}
	if m.LinodeTokenEnv != "LINODE_TOKEN" {
		t.Errorf("linode_token_env: got %q want %q", m.LinodeTokenEnv, "LINODE_TOKEN")
	}

	if len(m.Machines) != 3 {
		t.Fatalf("machines: got %d want 3", len(m.Machines))
	}
	if m.Machines[0].Name != "gateway-host" || m.Machines[0].Type != data.MachineTypeLinode {
		t.Errorf("machines[0]: %+v", m.Machines[0])
	}

	if len(m.Gateways) != 1 {
		t.Fatalf("gateways: got %d want 1", len(m.Gateways))
	}
	gw := m.Gateways[0]
	if gw.Name != "gateway" || gw.Reference != "gateway-host" {
		t.Errorf("gateway: %+v", gw)
	}
	if gw.Networking == nil || gw.Networking.Mode != "headscale" {
		t.Fatalf("gateway networking: %+v", gw.Networking)
	}
	if gw.Networking.PublicHostname == nil || gw.Networking.PublicHostname.Strategy != "sslip" {
		t.Errorf("public_hostname: %+v", gw.Networking.PublicHostname)
	}
	if gw.Networking.PreauthKeySource != "file" {
		t.Errorf("preauth_key_source: got %q want file", gw.Networking.PreauthKeySource)
	}
	if len(gw.Channels) != 6 {
		t.Errorf("channels: got %d want 6", len(gw.Channels))
	}
	var defaultTelegram string
	for _, ch := range gw.Channels {
		if ch.Default {
			defaultTelegram = ch.Name
			break
		}
	}
	if defaultTelegram != "telegram-main" {
		t.Errorf("default telegram channel: got %q want telegram-main", defaultTelegram)
	}

	if len(m.Agents) != 6 {
		t.Fatalf("agents: got %d want 6", len(m.Agents))
	}
	var mainAgent *data.Agent
	for i := range m.Agents {
		if m.Agents[i].ID == "main" {
			mainAgent = &m.Agents[i]
			break
		}
	}
	if mainAgent == nil {
		t.Fatal(`agent id "main" not found`)
	}
	if mainAgent.Model.Primary != "anthropic/claude-sonnet-4-6" {
		t.Errorf("main model primary: %q", mainAgent.Model.Primary)
	}
	if mainAgent.Tools == nil || mainAgent.Tools.Elevated == nil || mainAgent.Tools.Elevated.Enabled == nil || !*mainAgent.Tools.Elevated.Enabled {
		t.Errorf("main elevated tools: %+v", mainAgent.Tools)
	}

	var techWriter *data.Agent
	for i := range m.Agents {
		if m.Agents[i].ID == "tech-writer" {
			techWriter = &m.Agents[i]
			break
		}
	}
	if techWriter == nil {
		t.Fatal(`agent id "tech-writer" not found`)
	}
	if len(techWriter.Model.Fallbacks) != 1 || techWriter.Model.Fallbacks[0] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("tech-writer fallbacks: %#v", techWriter.Model.Fallbacks)
	}
	// Runtime pins are per model ref, so only the primary is pinned here
	// while the fallback keeps openclaw's default runtime selection.
	if got := techWriter.Models["anthropic/claude-opus-4-6"].Runtime; got != "claude-cli" {
		t.Errorf("tech-writer opus runtime: got %q want claude-cli", got)
	}
	if _, pinned := techWriter.Models["anthropic/claude-sonnet-4-6"]; pinned {
		t.Errorf("tech-writer sonnet should not be pinned: %#v", techWriter.Models)
	}

	if len(m.Nodes) != 2 {
		t.Fatalf("nodes: got %d want 2", len(m.Nodes))
	}
	if m.Nodes[0].ExecPolicy == nil || m.Nodes[0].ExecPolicy.Ask != "off" {
		t.Errorf("node exec_policy: %+v", m.Nodes[0].ExecPolicy)
	}

	if len(m.Automations) != 3 {
		t.Fatalf("automations: got %d want 3", len(m.Automations))
	}
	if m.Automations[0].Name != "gvm-go" || !m.Automations[0].Manual {
		t.Errorf("automation[0]: %+v", m.Automations[0])
	}
}
