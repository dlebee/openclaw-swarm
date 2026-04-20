package provisioning

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

type mockProvider struct {
	kind         string
	listByTag    map[string][]hosting.Instance
	listTagErr   error
	created      *hosting.CreateInstanceOpts
	createInst   *hosting.Instance
	createErr    error
	waitInst     *hosting.Instance
	waitErr      error
	deleteCalled []string
}

func (m *mockProvider) Kind() string {
	if m.kind != "" {
		return m.kind
	}
	return "mock"
}

func (m *mockProvider) CreateInstance(ctx context.Context, opts hosting.CreateInstanceOpts) (*hosting.Instance, error) {
	_ = ctx
	m.created = &opts
	if m.createErr != nil {
		return nil, m.createErr
	}
	if m.createInst != nil {
		return m.createInst, nil
	}
	return &hosting.Instance{
		Provider:   m.Kind(),
		ResourceID: "123",
		Label:      opts.Label,
		Region:     opts.Region,
		Status:     "provisioning",
	}, nil
}

func (m *mockProvider) DeleteInstance(ctx context.Context, resourceID string) error {
	_ = ctx
	m.deleteCalled = append(m.deleteCalled, resourceID)
	return nil
}

func (m *mockProvider) WaitRunning(ctx context.Context, resourceID string) (*hosting.Instance, error) {
	_ = ctx
	_ = resourceID
	if m.waitErr != nil {
		return nil, m.waitErr
	}
	if m.waitInst != nil {
		return m.waitInst, nil
	}
	return &hosting.Instance{
		Provider:   m.Kind(),
		ResourceID: "123",
		Status:     "running",
		PublicIPv4: "203.0.113.10",
	}, nil
}

func (m *mockProvider) ListByTag(ctx context.Context, tag string) ([]hosting.Instance, error) {
	_ = ctx
	if m.listTagErr != nil {
		return nil, m.listTagErr
	}
	if m.listByTag == nil {
		return nil, nil
	}
	return m.listByTag[tag], nil
}

func TestAddPhase_describe(t *testing.T) {
	p := scaffold.New()
	targets := BuildMachineTargets([]manifestdata.Machine{
		{Name: "web", Type: manifestdata.MachineTypeLinode},
		{Name: "jump", Type: manifestdata.MachineTypeSSH},
	})
	AddPhase(p, targets, Options{Provider: &mockProvider{}})

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}
	desc, err := ex.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(desc, "provisioning") {
		t.Fatalf("describe should mention provisioning: %q", desc)
	}
	if !strings.Contains(desc, "create machine") {
		t.Fatalf("describe should mention create machine step: %q", desc)
	}
	if !strings.Contains(desc, "authorized keys") {
		t.Fatalf("describe should mention authorized keys step: %q", desc)
	}
}

func TestAddPhase_concurrencyCappedAtFive(t *testing.T) {
	p := scaffold.New()
	machines := make([]manifestdata.Machine, 10)
	for i := range machines {
		machines[i] = manifestdata.Machine{
			Name: "m" + strconv.Itoa(i),
			Type: manifestdata.MachineTypeLinode,
		}
	}
	ph := AddPhase(p, BuildMachineTargets(machines), Options{Provider: &mockProvider{}})
	if ph.Concurrency != 5 {
		t.Fatalf("want concurrency 5, got %d", ph.Concurrency)
	}
}

func TestAddPhase_concurrencyBelowCap(t *testing.T) {
	p := scaffold.New()
	ph := AddPhase(p, BuildMachineTargets([]manifestdata.Machine{
		{Name: "a", Type: manifestdata.MachineTypeLinode},
		{Name: "b", Type: manifestdata.MachineTypeLinode},
		{Name: "c", Type: manifestdata.MachineTypeLinode},
	}), Options{Provider: &mockProvider{}})
	if ph.Concurrency != 3 {
		t.Fatalf("want concurrency 3, got %d", ph.Concurrency)
	}
}

func TestExecute_populatesPayload(t *testing.T) {
	p := scaffold.New()
	AddPhase(p, BuildMachineTargets([]manifestdata.Machine{
		{
			Name:   "web",
			Type:   manifestdata.MachineTypeLinode,
			Region: "us-east",
			SKU:    "g6-nanode-1",
			Image:  "linode/debian12",
		},
	}), Options{
		Provider:  &mockProvider{},
		Prefix:    "demo",
		SSHPubKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI",
	})

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	var mt *MachineTarget
	for _, ph := range p.Phases {
		for _, tgt := range ph.Targets {
			if m, ok := tgt.Payload.(*MachineTarget); ok && m.Spec.Name == "web" {
				mt = m
			}
		}
	}
	if mt == nil {
		t.Fatal("machine target not found")
	}

	if err := ex.Execute(context.Background(), scaffold.ExecuteOptions{Progress: progress.Noop{}}); err != nil {
		t.Fatal(err)
	}
	if mt.Instance == nil {
		t.Fatal("expected instance on payload")
	}
	if mt.Instance.PublicIPv4 != "203.0.113.10" {
		t.Fatalf("IPv4: got %q", mt.Instance.PublicIPv4)
	}
}

func TestCheck_satisfiedWhenExisting(t *testing.T) {
	wantLabel := machineLabel("pfx", "web")
	prefixTag := clawsPrefixTag("pfx")
	perMachineTag := machineTag("pfx", "web")
	provider := &mockProvider{
		listByTag: map[string][]hosting.Instance{
			prefixTag: {{
				Provider:   "mock",
				ResourceID: "999",
				Label:      wantLabel,
				Region:     "us-east",
				PublicIPv4: "198.51.100.1",
				Status:     "running",
				Tags:       []string{prefixTag, perMachineTag},
			}},
		},
	}
	p := scaffold.New()
	AddPhase(p, BuildMachineTargets([]manifestdata.Machine{
		{Name: "web", Type: manifestdata.MachineTypeLinode, Region: "us-east", SKU: "g6-nanode-1", Image: "linode/debian12"},
	}), Options{
		Provider:  provider,
		Prefix:    "pfx",
		SSHPubKey: "ssh-rsa AAAA",
	})

	ex, err := p.Build()
	if err != nil {
		t.Fatal(err)
	}

	var mt *MachineTarget
	var targets []scaffold.Target
	for _, ph := range p.Phases {
		for _, tgt := range ph.Targets {
			if m, ok := tgt.Payload.(*MachineTarget); ok {
				mt = m
				targets = append(targets, tgt)
			}
		}
	}

	// Mirror the production apply flow: run the pre-probe hosted-instance
	// resolver before Execute so MachineTarget.Instance is populated by a
	// dedicated resolver pass (rather than by Check as a side effect).
	// Check's purity rule (see .cursor/rules/plan-checks-are-pure.mdc)
	// forbids it from mutating the payload.
	ctx := scaffold.EnsurePlanCache(context.Background())
	if err := ResolveHostedInstances(ctx, provider, "pfx", targets); err != nil {
		t.Fatalf("resolve hosted: %v", err)
	}
	if mt.Instance == nil || mt.Instance.ResourceID != "999" {
		t.Fatalf("expected existing instance from pre-probe resolver, got %+v", mt.Instance)
	}

	if err := ex.Execute(ctx, scaffold.ExecuteOptions{Progress: progress.Noop{}}); err != nil {
		t.Fatal(err)
	}
	if mt.Instance == nil || mt.Instance.ResourceID != "999" {
		t.Fatalf("expected existing instance after Execute, got %+v", mt.Instance)
	}
}

func TestResolveMachineStatus_cacheShared(t *testing.T) {
	wantLabel := machineLabel("pfx", "web")
	prefixTag := clawsPrefixTag("pfx")
	provider := &mockProvider{
		listByTag: map[string][]hosting.Instance{
			prefixTag: {{
				Provider:   "mock",
				ResourceID: "1",
				Label:      wantLabel,
				PublicIPv4: "203.0.113.5",
			}},
		},
	}
	ctx := scaffold.EnsurePlanCache(context.Background())
	mt := &MachineTarget{Spec: manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode}}

	s1, err := ResolveMachineStatus(ctx, provider, "pfx", mt)
	if err != nil {
		t.Fatal(err)
	}
	if !s1.Exists || s1.Instance == nil || s1.Instance.ResourceID != "1" {
		t.Fatalf("unexpected first resolve: %+v", s1)
	}

	// Swap the provider's data out from under us. The second call should
	// still return the cached snapshot, proving the cache is shared and
	// read-through (cold == hot).
	provider.listByTag = nil
	s2, err := ResolveMachineStatus(ctx, provider, "pfx", mt)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Exists || s2.Instance == nil || s2.Instance.ResourceID != "1" {
		t.Fatalf("cache miss on second resolve: %+v", s2)
	}
}

func TestCreateMachine_Check_doesNotMutatePayloadOrCache(t *testing.T) {
	wantLabel := machineLabel("pfx", "web")
	prefixTag := clawsPrefixTag("pfx")
	provider := &mockProvider{
		listByTag: map[string][]hosting.Instance{
			prefixTag: {{
				Provider:   "mock",
				ResourceID: "42",
				Label:      wantLabel,
				PublicIPv4: "203.0.113.9",
			}},
		},
	}
	step := NewCreateMachineStep(Options{Provider: provider, Prefix: "pfx", SSHPubKey: "ssh-rsa AAAA"})
	mt := &MachineTarget{Spec: manifestdata.Machine{Name: "web", Type: manifestdata.MachineTypeLinode}}
	ctx := scaffold.EnsurePlanCache(context.Background())

	satisfied, err := step.Check(ctx, scaffold.Target{ID: "web", Payload: mt})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !satisfied {
		t.Fatalf("expected satisfied=true for an existing instance")
	}
	if mt.Instance != nil {
		t.Fatalf("Check must NOT mutate payload; got mt.Instance=%+v", mt.Instance)
	}
	if h, ok := scaffold.LookupPlanMachineHost(ctx, "web"); ok {
		t.Fatalf("Check must NOT write RecordPlanMachineHost; got %q", h)
	}
	if _, known := scaffold.DoesMachineExist(ctx, "web"); known {
		t.Fatal("Check must NOT write RecordPlanMachineExists")
	}
}
