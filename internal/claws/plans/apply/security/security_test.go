package security

import (
	"context"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// allSteps returns every security step for table-driven tests.
func allSteps(opts Options) []scaffold.Step {
	return []scaffold.Step{
		NewInstallPackagesStep(opts),
		NewEnableUFWStep(opts),
		NewEnableFail2banStep(opts),
		NewEnableUnattendedUpgradesStep(opts),
	}
}

func TestApplicable_linodeMachine(t *testing.T) {
	for _, s := range allSteps(Options{}) {
		ok, err := s.Applicable(context.Background(), scaffold.Target{
			ID:      "web",
			Payload: &provisioning.MachineTarget{Spec: manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode}},
		})
		if err != nil {
			t.Fatalf("%s: %v", s.Name(), err)
		}
		if !ok {
			t.Fatalf("%s: expected applicable for Linode", s.Name())
		}
	}
}

func TestApplicable_sshMachineSkipped(t *testing.T) {
	for _, s := range allSteps(Options{}) {
		ok, err := s.Applicable(context.Background(), scaffold.Target{
			ID:      "jump",
			Payload: &provisioning.MachineTarget{Spec: manifestdata.Machine{Name: "jump", Type: manifestdata.MachineTypeSSH}},
		})
		if err != nil {
			t.Fatalf("%s: %v", s.Name(), err)
		}
		if ok {
			t.Fatalf("%s: expected not applicable for SSH machine", s.Name())
		}
	}
}

func TestApplicable_nilPayload(t *testing.T) {
	for _, s := range allSteps(Options{}) {
		ok, err := s.Applicable(context.Background(), scaffold.Target{ID: "x", Payload: nil})
		if err != nil {
			t.Fatalf("%s: %v", s.Name(), err)
		}
		if ok {
			t.Fatalf("%s: expected not applicable for nil payload", s.Name())
		}
	}
}

func TestCheck_noInstanceNotSatisfied(t *testing.T) {
	opts := Options{
		SSHDial: func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
			t.Fatal("dial should not be called without instance")
			return nil, nil
		},
	}
	for _, s := range allSteps(opts) {
		satisfied, err := s.Check(context.Background(), scaffold.Target{
			ID: "web",
			Payload: &provisioning.MachineTarget{
				Spec:     manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
				Instance: nil,
			},
		})
		if err != nil || satisfied {
			t.Fatalf("%s: want not satisfied, no err; satisfied=%v err=%v", s.Name(), satisfied, err)
		}
	}
}

func TestCheck_emptyHostNotSatisfied(t *testing.T) {
	opts := Options{
		SSHDial: func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
			t.Fatal("dial should not be called for empty host")
			return nil, nil
		},
	}
	for _, s := range allSteps(opts) {
		satisfied, err := s.Check(context.Background(), scaffold.Target{
			ID: "web",
			Payload: &provisioning.MachineTarget{
				Spec:     manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
				Instance: &hosting.Instance{PublicIPv4: ""},
			},
		})
		if err != nil || satisfied {
			t.Fatalf("%s: want not satisfied, no err; satisfied=%v err=%v", s.Name(), satisfied, err)
		}
	}
}

func TestExecute_noHost(t *testing.T) {
	for _, s := range allSteps(Options{}) {
		err := s.Execute(context.Background(), scaffold.Target{
			ID: "web",
			Payload: &provisioning.MachineTarget{
				Spec:     manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode},
				Instance: nil,
			},
		})
		if err == nil {
			t.Fatalf("%s: expected error for missing host", s.Name())
		}
	}
}

func TestMachineHost_instanceIPv4(t *testing.T) {
	mt := &provisioning.MachineTarget{
		Spec:     manifestdata.Machine{Name: "web", Host: "static.example.com"},
		Instance: &hosting.Instance{PublicIPv4: "203.0.113.1"},
	}
	if got := machineHost(context.Background(), mt); got != "203.0.113.1" {
		t.Fatalf("machineHost = %q, want 203.0.113.1", got)
	}
}

func TestMachineHost_fallbackToSpecHost(t *testing.T) {
	mt := &provisioning.MachineTarget{
		Spec:     manifestdata.Machine{Name: "jump", Host: "static.example.com"},
		Instance: nil,
	}
	if got := machineHost(context.Background(), mt); got != "static.example.com" {
		t.Fatalf("machineHost = %q, want static.example.com", got)
	}
}

func TestMachineHost_emptyWhenNone(t *testing.T) {
	mt := &provisioning.MachineTarget{Spec: manifestdata.Machine{Name: "x"}, Instance: nil}
	if got := machineHost(context.Background(), mt); got != "" {
		t.Fatalf("machineHost = %q, want empty", got)
	}
}

func TestMachineSSHPort_default(t *testing.T) {
	if got := machineSSHPort(manifestdata.Machine{}); got != 22 {
		t.Fatalf("machineSSHPort = %d, want 22", got)
	}
}

func TestMachineSSHPort_custom(t *testing.T) {
	if got := machineSSHPort(manifestdata.Machine{SSHPort: 2222}); got != 2222 {
		t.Fatalf("machineSSHPort = %d, want 2222", got)
	}
}

func TestMachineBootstrapUser_defaultsToRoot(t *testing.T) {
	if got := machineBootstrapUser(manifestdata.Machine{}); got != "root" {
		t.Fatalf("machineBootstrapUser = %q, want root", got)
	}
}

func TestMachineBootstrapUser_ignoresAgentUser(t *testing.T) {
	// Security dials the bootstrap identity, not the agent — even if only
	// agent_user is set, security phase must not smuggle that in as the
	// bootstrap login.
	if got := machineBootstrapUser(manifestdata.Machine{AgentUser: "agent"}); got != "root" {
		t.Fatalf("machineBootstrapUser = %q, want root (agent_user must not leak into bootstrap)", got)
	}
}

func TestMachineBootstrapUser_explicit(t *testing.T) {
	m := manifestdata.Machine{BootstrapUser: "ubuntu", AgentUser: "agent"}
	if got := machineBootstrapUser(m); got != "ubuntu" {
		t.Fatalf("machineBootstrapUser = %q, want ubuntu", got)
	}
}

func TestAddPhase_stepCount(t *testing.T) {
	p := scaffold.New()
	ph := AddPhase(p, provisioning.BuildMachineTargets([]manifestdata.Machine{
		{Name: "web", Type: manifestdata.MachineTypeLinode},
	}), Options{})
	if len(ph.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(ph.Steps))
	}
	want := []string{"install-security-packages", "enable-ufw", "enable-fail2ban", "enable-unattended-upgrades"}
	for i, s := range ph.Steps {
		if s.Name() != want[i] {
			t.Fatalf("step[%d] = %q, want %q", i, s.Name(), want[i])
		}
	}
}
