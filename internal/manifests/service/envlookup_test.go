package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

func TestLookupEnvFromManifest_envFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yml")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LINODE_TOKEN=secret-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &data.Manifest{EnvFile: ".env", LinodeTokenEnv: "LINODE_TOKEN"}
	got, err := LookupEnvFromManifest(manifestPath, m, "LINODE_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-from-file" {
		t.Fatalf("got %q", got)
	}
}

func TestLookupEnvFromManifest_processOverridesFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yml")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LINODE_TOKEN=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINODE_TOKEN", "from-process")
	m := &data.Manifest{EnvFile: ".env", LinodeTokenEnv: "LINODE_TOKEN"}
	got, err := LookupEnvFromManifest(manifestPath, m, "LINODE_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-process" {
		t.Fatalf("process env should win: got %q", got)
	}
}
