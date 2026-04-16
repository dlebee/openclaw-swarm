package ufw

import (
	"testing"
)

func TestIsActive_nilClient(t *testing.T) {
	_, err := IsActive(nil)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestAllowPort_nilClient(t *testing.T) {
	if err := AllowPort(nil, 22, "tcp"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestAllowPort_invalidPort(t *testing.T) {
	if err := AllowPort(nil, 0, "tcp"); err == nil {
		t.Fatal("expected error for port 0")
	}
	if err := AllowPort(nil, 70000, "tcp"); err == nil {
		t.Fatal("expected error for port > 65535")
	}
}

func TestEnable_nilClient(t *testing.T) {
	if err := Enable(nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}
