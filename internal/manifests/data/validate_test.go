package data

import (
	"strings"
	"testing"
)

func TestValidateManifest_SelfOptIn(t *testing.T) {
	t.Parallel()

	t.Run("rejects self without allow_self", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Automations: []Automation{{
				Name:     "build",
				Machines: []string{"self"},
				Steps:    []AutomationStep{{Name: "s", Execute: "echo hi"}},
			}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "allow_self") {
			t.Fatalf("want allow_self error, got %v", err)
		}
	})

	t.Run("accepts self with allow_self", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			AllowSelf: true,
			Automations: []Automation{{
				Name:     "build",
				Machines: []string{"self"},
				Steps:    []AutomationStep{{Name: "s", Execute: "echo hi"}},
			}},
		}
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects real machine named self", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Machines: []Machine{{Name: "self", Type: MachineTypeSSH}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("want reserved error, got %v", err)
		}
	})
}

func TestValidateManifest_SCPSteps(t *testing.T) {
	t.Parallel()

	base := func(step AutomationStep, allowSelf bool) *Manifest {
		return &Manifest{
			AllowSelf: allowSelf,
			Automations: []Automation{{
				Name:     "ship",
				Machines: []string{"node-host"},
				Steps:    []AutomationStep{step},
			}},
		}
	}

	t.Run("valid upload", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name:        "push",
			Kind:        StepKindSCPUpload,
			Source:      "/tmp/bin",
			Destination: "/opt/app/bin",
			Mode:        "0755",
		}, true)
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("needs allow_self", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "push", Kind: StepKindSCPUpload,
			Source: "a", Destination: "b",
		}, false)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "allow_self") {
			t.Fatalf("want allow_self error, got %v", err)
		}
	})

	t.Run("needs source", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "push", Kind: StepKindSCPUpload,
			Destination: "b",
		}, true)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "source") {
			t.Fatalf("want source error, got %v", err)
		}
	})

	t.Run("needs destination", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "push", Kind: StepKindSCPUpload,
			Source: "a",
		}, true)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "destination") {
			t.Fatalf("want destination error, got %v", err)
		}
	})

	t.Run("rejects execute script", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "push", Kind: StepKindSCPUpload,
			Source: "a", Destination: "b",
			Execute: "echo nope",
		}, true)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "execute") {
			t.Fatalf("want execute error, got %v", err)
		}
	})

	t.Run("rejects self in machines for scp", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			AllowSelf: true,
			Automations: []Automation{{
				Name:     "ship",
				Machines: []string{"self", "node-host"},
				Steps: []AutomationStep{{
					Name: "push", Kind: StepKindSCPUpload,
					Source: "a", Destination: "b",
				}},
			}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "self is implicit") {
			t.Fatalf("want self-implicit error, got %v", err)
		}
	})

	t.Run("needs at least one remote", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			AllowSelf: true,
			Automations: []Automation{{
				Name:     "ship",
				Machines: []string{},
				Steps: []AutomationStep{{
					Name: "push", Kind: StepKindSCPUpload,
					Source: "a", Destination: "b",
				}},
			}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "remote machine") {
			t.Fatalf("want remote-machine error, got %v", err)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "push", Kind: StepKindSCPUpload,
			Source: "a", Destination: "b",
			Mode: "rwxrwxrwx",
		}, true)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "octal") {
			t.Fatalf("want octal error, got %v", err)
		}
	})

	t.Run("if_changed ok on scp.upload", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "push", Kind: StepKindSCPUpload,
			Source: "a", Destination: "b",
			IfChanged: true,
		}, true)
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("if_changed rejected on scp.download", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "pull", Kind: StepKindSCPDownload,
			Source: "a", Destination: "b",
			IfChanged: true,
		}, true)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "if_changed") {
			t.Fatalf("want if_changed error, got %v", err)
		}
	})

	t.Run("if_changed rejected on bash", func(t *testing.T) {
		t.Parallel()
		m := base(AutomationStep{
			Name: "run", Kind: StepKindBash,
			Execute:   "echo hi",
			IfChanged: true,
		}, true)
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "if_changed") {
			t.Fatalf("want if_changed error, got %v", err)
		}
	})
}

func TestValidateManifest_MainAgentWorkspace(t *testing.T) {
	t.Parallel()

	t.Run("rejects workspace override for main", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Agents: []Agent{{
				ID:        "main",
				Workspace: "~/.openclaw/workspace-custom",
			}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "workspace override") {
			t.Fatalf("want workspace-override error, got %v", err)
		}
	})

	t.Run("rejects workspace override for Main (case-insensitive)", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Agents: []Agent{{
				ID:        "Main",
				Workspace: "~/.openclaw/workspace-custom",
			}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "workspace override") {
			t.Fatalf("want workspace-override error, got %v", err)
		}
	})

	t.Run("accepts main without workspace override", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Agents: []Agent{{
				ID: "main",
			}},
		}
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("accepts non-main agent with workspace", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Agents: []Agent{{
				ID:        "dev",
				Workspace: "~/.openclaw/workspace-dev",
			}},
		}
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}


func TestValidateStep_UnknownKind(t *testing.T) {
	t.Parallel()
	m := &Manifest{
		Automations: []Automation{{
			Name:     "auto",
			Machines: []string{"node"},
			Steps:    []AutomationStep{{Name: "s", Kind: "wat", Execute: "echo"}},
		}},
	}
	err := ValidateManifest(m)
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("want unknown-kind error, got %v", err)
	}
}

func TestValidateNodeGatewayColocation(t *testing.T) {
	t.Parallel()

	t.Run("rejects node on same machine as its gateway", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Machines: []Machine{{Name: "main-host", Type: MachineTypeSSH}},
			Gateways: []Gateway{{Name: "gateway", Reference: "main-host"}},
			Nodes:    []Node{{Name: "exec-node", Gateway: "gateway", Reference: "main-host"}},
		}
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "separate machines") {
			t.Fatalf("want colocation error, got %v", err)
		}
	})

	t.Run("accepts node on different machine from gateway", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Machines: []Machine{
				{Name: "gw-host", Type: MachineTypeSSH},
				{Name: "node-host", Type: MachineTypeSSH},
			},
			Gateways: []Gateway{{Name: "gateway", Reference: "gw-host"}},
			Nodes:    []Node{{Name: "exec-node", Gateway: "gateway", Reference: "node-host"}},
		}
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("accepts manifest with no nodes", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Machines: []Machine{{Name: "main-host", Type: MachineTypeSSH}},
			Gateways: []Gateway{{Name: "gateway", Reference: "main-host"}},
		}
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		want  uint32
		okish bool
	}{
		{"", 0, false},
		{"0755", 0o755, true},
		{"755", 0o755, true},
		{"0600", 0o600, true},
		{"nope", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseMode(c.in)
		if ok != c.okish || got != c.want {
			t.Fatalf("ParseMode(%q) = (%o, %v); want (%o, %v)", c.in, got, ok, c.want, c.okish)
		}
	}
}

func TestValidateManifest_AgentModels(t *testing.T) {
	t.Parallel()

	agentWith := func(models AgentModels) *Manifest {
		return &Manifest{Agents: []Agent{{
			ID:      "dev",
			Gateway: "gateway",
			Model:   AgentModel{Primary: "anthropic/claude-opus-5"},
			Models:  models,
		}}}
	}

	t.Run("accepts a runtime pin", func(t *testing.T) {
		t.Parallel()
		m := agentWith(AgentModels{"anthropic/claude-opus-5": {Runtime: "claude-cli"}})
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("accepts a ref outside primary/fallbacks", func(t *testing.T) {
		t.Parallel()
		// Legitimate: the agent can reach this model via an in-chat
		// /model switch, and the pin should apply when it does.
		m := agentWith(AgentModels{"anthropic/claude-haiku-4-5": {Runtime: "claude-cli"}})
		if err := ValidateManifest(m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects a blank ref", func(t *testing.T) {
		t.Parallel()
		m := agentWith(AgentModels{"  ": {Runtime: "claude-cli"}})
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "blank model ref") {
			t.Fatalf("want blank ref error, got %v", err)
		}
	})

	t.Run("rejects an entry with no fields", func(t *testing.T) {
		t.Parallel()
		m := agentWith(AgentModels{"anthropic/claude-opus-5": {}})
		err := ValidateManifest(m)
		if err == nil || !strings.Contains(err.Error(), "sets no fields") {
			t.Fatalf("want empty-entry error, got %v", err)
		}
		if !strings.Contains(err.Error(), `agent "dev"`) {
			t.Fatalf("error should name the agent, got %v", err)
		}
	})
}
