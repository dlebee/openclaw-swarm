package systemd

import "testing"

func TestValidateUnit(t *testing.T) {
	if err := validateUnit(""); err == nil {
		t.Fatal("expected error for empty unit")
	}
	if err := validateUnit("  "); err == nil {
		t.Fatal("expected error for blank unit")
	}
	if err := validateUnit("foo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCtlPrefix(t *testing.T) {
	prefix, env := ctlPrefix(false)
	if prefix != "sudo systemctl" {
		t.Errorf("system prefix: got %q", prefix)
	}
	if env != "" {
		t.Errorf("system env should be empty, got %q", env)
	}

	prefix, env = ctlPrefix(true)
	if prefix != "systemctl --user" {
		t.Errorf("user prefix: got %q", prefix)
	}
	if env == "" {
		t.Error("user env should set XDG_RUNTIME_DIR")
	}
}

func TestUnitDir(t *testing.T) {
	if got := unitDir(false); got != "/etc/systemd/system" {
		t.Errorf("system dir: got %q", got)
	}
	if got := unitDir(true); got != "$HOME/.config/systemd/user" {
		t.Errorf("user dir: got %q", got)
	}
}

func TestEnable_emptyUnit(t *testing.T) {
	if err := Enable(nil, "", false); err == nil {
		t.Fatal("expected error for empty unit")
	}
}

func TestEnableNow_emptyUnit(t *testing.T) {
	if err := EnableNow(nil, "  ", false); err == nil {
		t.Fatal("expected error for blank unit")
	}
}

func TestStart_emptyUnit(t *testing.T) {
	if err := Start(nil, "", true); err == nil {
		t.Fatal("expected error")
	}
}

func TestStop_emptyUnit(t *testing.T) {
	if err := Stop(nil, "", false); err == nil {
		t.Fatal("expected error")
	}
}

func TestRestart_emptyUnit(t *testing.T) {
	if err := Restart(nil, "", false); err == nil {
		t.Fatal("expected error")
	}
}

func TestIsActive_emptyUnit(t *testing.T) {
	_, err := IsActive(nil, "", false)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRemove_emptyUnit(t *testing.T) {
	if err := Remove(nil, "", false); err == nil {
		t.Fatal("expected error")
	}
}

func TestLogs_emptyUnit(t *testing.T) {
	_, err := Logs(nil, "", false, 50)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEnableLingering_emptyUser(t *testing.T) {
	if err := EnableLingering(nil, ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestWrite_emptyUnit(t *testing.T) {
	if err := Write(nil, UnitSpec{Name: ""}); err == nil {
		t.Fatal("expected error")
	}
}
