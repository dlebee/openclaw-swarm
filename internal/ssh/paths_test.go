package ssh

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExpandPath_tilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExpandPath("~/a/b")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "a", "b")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandPath_tildeBackslash_windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("~\\ expansion is Windows-specific")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExpandPath(`~\AppData\Roaming\claws\ssh\keys\dev\key.pub`)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "AppData", "Roaming", "claws", "ssh", "keys", "dev", "key.pub")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandPath_cleanNonTilde(t *testing.T) {
	base := t.TempDir()
	in := filepath.Join(base, "x", "..", "y")
	got, err := ExpandPath(in)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(in)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
