package user

import (
	"testing"
)

func TestExists_nilClient(t *testing.T) {
	_, err := Exists(nil, "agent")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestExists_emptyName(t *testing.T) {
	_, err := Exists(nil, "")
	if err == nil {
		t.Fatal("expected error for empty username")
	}
}

func TestExists_root(t *testing.T) {
	_, err := Exists(nil, "root")
	if err == nil {
		t.Fatal("expected error for root")
	}
}

func TestEnsure_nilClient(t *testing.T) {
	if err := Ensure(nil, "agent"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestEnsure_emptyName(t *testing.T) {
	if err := Ensure(nil, ""); err == nil {
		t.Fatal("expected error for empty username")
	}
}

func TestEnsure_root(t *testing.T) {
	if err := Ensure(nil, "root"); err == nil {
		t.Fatal("expected error for root")
	}
}

func TestValidateUsername_whitespace(t *testing.T) {
	if err := validateUsername("  "); err == nil {
		t.Fatal("expected error for whitespace-only username")
	}
}
