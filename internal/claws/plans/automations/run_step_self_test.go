package automations

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// selfTarget builds a scaffold.Target whose payload is the synthetic
// AutomationTarget we produce in buildTargets for the "self" reserved
// name. Kept inline rather than exported — there's no value in growing
// the public surface for a test helper.
func selfTarget() scaffold.Target {
	return scaffold.Target{
		ID: manifestdata.SelfMachineName,
		Payload: &AutomationTarget{MachineTarget: &provisioning.MachineTarget{
			Spec: manifestdata.Machine{
				Name: manifestdata.SelfMachineName,
				Type: manifestdata.MachineTypeSelf,
			},
		}},
	}
}

func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash not guaranteed on windows runners")
	}
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skipf("no /bin/bash available: %v", err)
	}
}

func TestDynamicStep_Self_Execute(t *testing.T) {
	t.Parallel()
	requireBash(t)

	tmp := t.TempDir()
	marker := filepath.Join(tmp, "marker")
	step := manifestdata.AutomationStep{
		Name:    "touch",
		Execute: "echo ok > " + marker + "\n",
	}
	d := NewDynamicStep(step, "", nil, Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := d.Execute(ctx, selfTarget()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not created: %v", err)
	}
	if strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("marker content: %q", b)
	}
}

func TestDynamicStep_Self_ApplicableAndCheck(t *testing.T) {
	t.Parallel()
	requireBash(t)

	// Each subtest gets its own context; sharing a parent ctx with
	// t.Parallel() children leads to subtle ordering with the parent's
	// deferred cancel. Keep it boring.
	newCtx := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("applicable: no script => true", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{Name: "s"}, "", nil, Options{})
		ok, err := d.Applicable(newCtx(t), selfTarget())
		if err != nil || !ok {
			t.Fatalf("want (true, nil); got (%v, %v)", ok, err)
		}
	})

	t.Run("applicable: script exit 0 => true", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{
			Name:       "s",
			Applicable: "exit 0\n",
		}, "", nil, Options{})
		ok, err := d.Applicable(newCtx(t), selfTarget())
		if err != nil || !ok {
			t.Fatalf("want (true, nil); got (%v, %v)", ok, err)
		}
	})

	t.Run("applicable: script exit 1 => false, no err", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{
			Name:       "s",
			Applicable: "exit 1\n",
		}, "", nil, Options{})
		ok, err := d.Applicable(newCtx(t), selfTarget())
		if err != nil || ok {
			t.Fatalf("want (false, nil); got (%v, %v)", ok, err)
		}
	})

	t.Run("check: no script => false (always execute)", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{Name: "s"}, "", nil, Options{})
		ok, err := d.Check(newCtx(t), selfTarget())
		if err != nil || ok {
			t.Fatalf("want (false, nil); got (%v, %v)", ok, err)
		}
	})

	t.Run("check: script exit 0 => already-satisfied true", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{
			Name:  "s",
			Check: "exit 0\n",
		}, "", nil, Options{})
		ok, err := d.Check(newCtx(t), selfTarget())
		if err != nil || !ok {
			t.Fatalf("want (true, nil); got (%v, %v)", ok, err)
		}
	})

	t.Run("check: script exit 1 => false, no err", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{
			Name:  "s",
			Check: "exit 1\n",
		}, "", nil, Options{})
		ok, err := d.Check(newCtx(t), selfTarget())
		if err != nil || ok {
			t.Fatalf("want (false, nil); got (%v, %v)", ok, err)
		}
	})
}

func TestDynamicStep_Self_ExecuteFailsSurfacesError(t *testing.T) {
	t.Parallel()
	requireBash(t)

	d := NewDynamicStep(manifestdata.AutomationStep{
		Name:    "boom",
		Execute: "echo nope >&2\nexit 3\n",
	}, "", nil, Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := d.Execute(ctx, selfTarget())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want wrapped stderr 'nope', got %v", err)
	}
}

func TestDynamicStep_Self_SCPUploadRejected(t *testing.T) {
	t.Parallel()
	// scp.* must never target self — validator blocks this at load time
	// but the Execute path has a defensive guard we exercise here.
	d := NewDynamicStep(manifestdata.AutomationStep{
		Name:        "push",
		Kind:        manifestdata.StepKindSCPUpload,
		Source:      "/tmp/a",
		Destination: "/tmp/b",
	}, "", nil, Options{})

	err := d.Execute(context.Background(), selfTarget())
	if err == nil || !strings.Contains(err.Error(), "cannot target self") {
		t.Fatalf("want guard error, got %v", err)
	}
}

func TestBuildTargets_SelfSynthesized(t *testing.T) {
	t.Parallel()
	auto := manifestdata.Automation{
		Name:     "a",
		Machines: []string{"self", "unknown", "node"},
	}
	mtByName := map[string]*provisioning.MachineTarget{
		"node": {Spec: manifestdata.Machine{Name: "node", Type: manifestdata.MachineTypeSSH, Host: "10.0.0.1"}},
	}
	got := buildTargets(auto, mtByName)
	if len(got) != 2 {
		t.Fatalf("want 2 targets (self + node); got %d: %v", len(got), got)
	}
	if got[0].ID != "self" {
		t.Fatalf("first target ID: %q", got[0].ID)
	}
	if !got[0].Payload.(*AutomationTarget).IsSelf() {
		t.Fatalf("first target should be self")
	}
	if got[1].ID != "node" {
		t.Fatalf("second target ID: %q", got[1].ID)
	}
	if got[1].Payload.(*AutomationTarget).IsSelf() {
		t.Fatalf("second target should NOT be self")
	}
}
