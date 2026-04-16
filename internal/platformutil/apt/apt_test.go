package apt

import (
	"testing"
)

func TestUpdate_nilClient(t *testing.T) {
	if err := Update(nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestInstall_nilClient(t *testing.T) {
	if err := Install(nil, "ufw"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestInstall_noPackages(t *testing.T) {
	if err := Install(nil); err == nil {
		t.Fatal("expected error for empty package list")
	}
}

func TestInstall_emptyPackageName(t *testing.T) {
	if err := Install(nil, "ufw", "  "); err == nil {
		t.Fatal("expected error for blank package name")
	}
}

func TestIsInstalled_nilClient(t *testing.T) {
	_, err := IsInstalled(nil, "ufw")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestIsInstalled_emptyPackage(t *testing.T) {
	_, err := IsInstalled(nil, "")
	if err == nil {
		t.Fatal("expected error for empty package name")
	}
}
