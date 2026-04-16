package systemctl

import (
	"testing"
)

func TestEnable_nilClient(t *testing.T) {
	if err := Enable(nil, "fail2ban"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestEnable_emptyUnit(t *testing.T) {
	if err := Enable(nil, ""); err == nil {
		t.Fatal("expected error for empty unit")
	}
}

func TestEnableNow_nilClient(t *testing.T) {
	if err := EnableNow(nil, "fail2ban"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestEnableNow_emptyUnit(t *testing.T) {
	if err := EnableNow(nil, "  "); err == nil {
		t.Fatal("expected error for blank unit")
	}
}

func TestRestart_nilClient(t *testing.T) {
	if err := Restart(nil, "fail2ban"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestRestart_emptyUnit(t *testing.T) {
	if err := Restart(nil, ""); err == nil {
		t.Fatal("expected error for empty unit")
	}
}

func TestIsActive_nilClient(t *testing.T) {
	_, err := IsActive(nil, "fail2ban")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestIsActive_emptyUnit(t *testing.T) {
	_, err := IsActive(nil, "")
	if err == nil {
		t.Fatal("expected error for empty unit")
	}
}
