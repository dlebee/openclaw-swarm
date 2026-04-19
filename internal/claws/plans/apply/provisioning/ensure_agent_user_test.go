package provisioning

import (
	"context"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

func TestEnsureAgentUser_Applicable_linodeWithUser(t *testing.T) {
	step := NewEnsureAgentUserStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "web",
		Payload: &MachineTarget{Spec: manifestdata.Machine{Type: manifestdata.MachineTypeLinode, AgentUser: "claw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected applicable for Linode with agent_user")
	}
}

func TestEnsureAgentUser_NotApplicable_ssh(t *testing.T) {
	step := NewEnsureAgentUserStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "jump",
		Payload: &MachineTarget{Spec: manifestdata.Machine{Type: manifestdata.MachineTypeSSH, AgentUser: "claw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be applicable to SSH machines")
	}
}

func TestEnsureAgentUser_NotApplicable_noAgentUser(t *testing.T) {
	step := NewEnsureAgentUserStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "web",
		Payload: &MachineTarget{Spec: manifestdata.Machine{Type: manifestdata.MachineTypeLinode, AgentUser: ""}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be applicable when agent_user is empty")
	}
}

func TestEnsureAgentUser_NotApplicable_root(t *testing.T) {
	step := NewEnsureAgentUserStep(Options{})
	ok, err := step.Applicable(context.Background(), scaffold.Target{
		ID:      "web",
		Payload: &MachineTarget{Spec: manifestdata.Machine{Type: manifestdata.MachineTypeLinode, AgentUser: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be applicable when agent_user is root")
	}
}

func TestEnsureAgentUser_Name(t *testing.T) {
	step := NewEnsureAgentUserStep(Options{})
	if step.Name() != "ensure-agent-user" {
		t.Fatalf("name = %q, want ensure-agent-user", step.Name())
	}
}

func TestNeedsAgentUser(t *testing.T) {
	tests := []struct {
		user string
		want bool
	}{
		{"", false},
		{"root", false},
		{"  ", false},
		{"  root  ", false},
		{"claw", true},
		{"agent", true},
	}
	for _, tt := range tests {
		got := needsAgentUser(manifestdata.Machine{AgentUser: tt.user})
		if got != tt.want {
			t.Errorf("needsAgentUser(%q) = %v, want %v", tt.user, got, tt.want)
		}
	}
}

func TestAddPhase_includesEnsureAgentUser(t *testing.T) {
	p := scaffold.New()
	targets := BuildMachineTargets([]manifestdata.Machine{
		{Name: "web", Type: manifestdata.MachineTypeLinode, AgentUser: "claw"},
	})
	ph := AddPhase(p, targets, Options{Provider: &mockProvider{}})

	want := []string{"create-machine", "authorize-ssh-key", "wait-cloud-init", "ensure-agent-user"}
	if len(ph.Steps) != len(want) {
		t.Fatalf("expected %d steps, got %d", len(want), len(ph.Steps))
	}
	for i, s := range ph.Steps {
		if s.Name() != want[i] {
			t.Fatalf("step[%d] = %q, want %q", i, s.Name(), want[i])
		}
	}
}
