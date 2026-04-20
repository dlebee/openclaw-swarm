package sshfile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalSHA256_file(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	payload := []byte("hello cache purity\n")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	want := sha256.Sum256(payload)
	got, err := LocalSHA256(path)
	if err != nil {
		t.Fatalf("LocalSHA256: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("sha mismatch: got %s want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestLocalSHA256_missing(t *testing.T) {
	t.Parallel()
	_, err := LocalSHA256(filepath.Join(t.TempDir(), "nope"))
	if !os.IsNotExist(err) {
		t.Fatalf("want IsNotExist, got %v", err)
	}
}

func TestLocalSHA256_dirRejected(t *testing.T) {
	t.Parallel()
	_, err := LocalSHA256(t.TempDir())
	if err == nil {
		t.Fatalf("want error for directory, got nil")
	}
}

func TestIsHex(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"deadbeef":       true,
		"":               true, // empty matches the for-range noop
		"DEADBEEF":       false,
		"abcdefg":        false,
		"0123456789abcd": true,
		"xyz":            false,
	}
	for in, want := range cases {
		if got := isHex(in); got != want {
			t.Errorf("isHex(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/home/agent/orchestrator-manifest.yaml": "'/home/agent/orchestrator-manifest.yaml'",
		"path with spaces":                       "'path with spaces'",
		"it's a file":                            `'it'\''s a file'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
