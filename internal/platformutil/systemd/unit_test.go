package systemd

import (
	"strings"
	"testing"
)

func TestUnitSpec_Render_SystemMode(t *testing.T) {
	spec := UnitSpec{
		Name:        "openclaw-gateway",
		Description: "OpenClaw Gateway",
		ExecStart:   "/usr/bin/openclaw gateway start",
		Env: map[string]string{
			"NODE_ENV":    "production",
			"OPENCLAW_ID": "gw-1",
		},
		Restart:    RestartAlways,
		RestartSec: 5,
		UserMode:   false,
	}
	got := spec.Render()

	if !strings.Contains(got, "Description=OpenClaw Gateway") {
		t.Error("missing description")
	}
	if !strings.Contains(got, "ExecStart=/usr/bin/openclaw gateway start") {
		t.Error("missing ExecStart")
	}
	if !strings.Contains(got, "Environment=NODE_ENV=production") {
		t.Error("missing NODE_ENV")
	}
	if !strings.Contains(got, "Environment=OPENCLAW_ID=gw-1") {
		t.Error("missing OPENCLAW_ID")
	}
	if !strings.Contains(got, "Restart=always") {
		t.Error("missing Restart=always")
	}
	if !strings.Contains(got, "RestartSec=5") {
		t.Error("missing RestartSec")
	}
	if !strings.Contains(got, "WantedBy=multi-user.target") {
		t.Error("expected multi-user.target for system mode")
	}
	if strings.Contains(got, "WantedBy=default.target") {
		t.Error("should not have default.target in system mode")
	}
	if strings.Contains(got, "StandardOutput") {
		t.Error("StandardOutput should be absent when LogPath is empty")
	}
}

func TestUnitSpec_Render_LogPath(t *testing.T) {
	spec := UnitSpec{
		Name:      "openclaw-gateway",
		ExecStart: "/usr/bin/env openclaw gateway",
		LogPath:   "/tmp/openclaw-gateway.log",
		UserMode:  true,
	}
	got := spec.Render()
	if !strings.Contains(got, "StandardOutput=append:/tmp/openclaw-gateway.log") {
		t.Error("missing StandardOutput")
	}
	if !strings.Contains(got, "StandardError=append:/tmp/openclaw-gateway.log") {
		t.Error("missing StandardError")
	}
}

func TestUnitSpec_Render_UserMode(t *testing.T) {
	spec := UnitSpec{
		Name:      "openclaw-node",
		ExecStart: "/home/oc/.openclaw/bin/openclaw node start",
		UserMode:  true,
	}
	got := spec.Render()

	if !strings.Contains(got, "Description=openclaw-node") {
		t.Error("default description should be Name")
	}
	if !strings.Contains(got, "Restart=always") {
		t.Error("default restart should be always")
	}
	if !strings.Contains(got, "WantedBy=default.target") {
		t.Error("expected default.target for user mode")
	}
	if strings.Contains(got, "RestartSec=") {
		t.Error("RestartSec=0 should be omitted")
	}
}

func TestUnitSpec_Render_EnvSorted(t *testing.T) {
	spec := UnitSpec{
		Name:      "test",
		ExecStart: "/bin/test",
		Env: map[string]string{
			"ZZZ": "3",
			"AAA": "1",
			"MMM": "2",
		},
	}
	got := spec.Render()
	aIdx := strings.Index(got, "Environment=AAA=1")
	mIdx := strings.Index(got, "Environment=MMM=2")
	zIdx := strings.Index(got, "Environment=ZZZ=3")
	if aIdx == -1 || mIdx == -1 || zIdx == -1 {
		t.Fatalf("missing env lines:\n%s", got)
	}
	if !(aIdx < mIdx && mIdx < zIdx) {
		t.Error("env lines should be sorted alphabetically")
	}
}

func TestUnitSpec_ServiceFileName(t *testing.T) {
	spec := UnitSpec{Name: "foo"}
	if got := spec.ServiceFileName(); got != "foo.service" {
		t.Errorf("got %q, want %q", got, "foo.service")
	}
}
