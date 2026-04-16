package destroy

import (
	"context"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

type mockProvider struct {
	list map[string][]hosting.Instance
	del  []string
}

func (m *mockProvider) Kind() string { return "mock" }

func (m *mockProvider) CreateInstance(ctx context.Context, opts hosting.CreateInstanceOpts) (*hosting.Instance, error) {
	return nil, nil
}

func (m *mockProvider) DeleteInstance(ctx context.Context, resourceID string) error {
	m.del = append(m.del, resourceID)
	return nil
}

func (m *mockProvider) WaitRunning(ctx context.Context, resourceID string) (*hosting.Instance, error) {
	return nil, nil
}

func (m *mockProvider) ListByTag(ctx context.Context, tag string) ([]hosting.Instance, error) {
	if m.list == nil {
		return nil, nil
	}
	return m.list[tag], nil
}

func TestListInstances_usesClawsPrefixTag(t *testing.T) {
	tag := provisioning.ClawsPrefixTag("demo")
	p := &mockProvider{
		list: map[string][]hosting.Instance{
			tag: {{ResourceID: "1", Label: "demo-web"}},
		},
	}
	got, err := ListInstances(context.Background(), p, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResourceID != "1" {
		t.Fatalf("got %+v", got)
	}
}

func TestDeleteInstances_order(t *testing.T) {
	p := &mockProvider{}
	instances := []hosting.Instance{
		{ResourceID: "10", Label: "a"},
		{ResourceID: "20", Label: "b"},
	}
	if err := DeleteInstances(context.Background(), p, instances, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.del) != 2 || p.del[0] != "10" || p.del[1] != "20" {
		t.Fatalf("del=%v", p.del)
	}
}
