package linode

import (
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

func TestInstancesWithExactTag(t *testing.T) {
	tag := "claws/dlebee-v2"
	in := []hosting.Instance{
		{ResourceID: "1", Tags: []string{"other", tag}},
		{ResourceID: "2", Tags: []string{"unrelated"}},
		{ResourceID: "3", Tags: []string{}},
	}
	out := instancesWithExactTag(in, tag)
	if len(out) != 1 || out[0].ResourceID != "1" {
		t.Fatalf("want one match, got %+v", out)
	}
}

func TestInstancesWithExactTag_emptyTag(t *testing.T) {
	out := instancesWithExactTag([]hosting.Instance{{Tags: []string{"a"}}}, "")
	if len(out) != 0 {
		t.Fatalf("want empty for empty tag, got %+v", out)
	}
}
