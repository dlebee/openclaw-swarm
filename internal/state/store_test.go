package state

import (
	"os"
	"path/filepath"
	"testing"

	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
)

func TestStoreManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	manifestPath := filepath.Join(dir, "m.yml")
	if err := writeFile(manifestPath, "prefix: test\n"); err != nil {
		t.Fatal(err)
	}

	s, err := Open(cfgPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetManifest("lab", manifestPath); err != nil {
		t.Fatalf("SetManifest: %v", err)
	}
	if err := s.Use("lab"); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if err := s.PutSSHIdentity("mine", clawssh.Identity{
		PublicKeyPath:  "~/.ssh/a.pub",
		PrivateKeyPath: "~/.ssh/a",
	}); err != nil {
		t.Fatalf("PutSSHIdentity: %v", err)
	}
	if err := s.UseSSHIdentity("mine"); err != nil {
		t.Fatalf("UseSSHIdentity: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Open(cfgPath)
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	if s2.CurrentManifestName() != "lab" {
		t.Fatalf("current: %q", s2.CurrentManifestName())
	}
	list := s2.ListManifests()
	if len(list) != 1 || list["lab"].Path == "" {
		t.Fatalf("list: %#v", list)
	}
	id, err := s2.ActiveSSHIdentity()
	if err != nil || id == nil || id.PrivateKeyPath != "~/.ssh/a" {
		t.Fatalf("auth: %#v err %v", id, err)
	}

	m, err := s2.LoadCurrentManifest()
	if err != nil {
		t.Fatalf("LoadCurrentManifest: %v", err)
	}
	if m.Prefix != "test" {
		t.Fatalf("manifest prefix: %q", m.Prefix)
	}
}

func TestMigrateV1FlatSSH(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `version: 1
manifests: {}
auth:
  ssh:
    public_key_path: ~/.ssh/a.pub
    private_key_path: ~/.ssh/a
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.ActiveSSHIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if id.PrivateKeyPath != "~/.ssh/a" || id.PublicKeyPath != "~/.ssh/a.pub" {
		t.Fatalf("migrated identity: %#v", id)
	}
	if s.SSHCurrent() != "default" {
		t.Fatalf("current: %q", s.SSHCurrent())
	}
}

func TestRemoveSSHIdentityDeletesActiveSwitchesOrClears(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	s, err := Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.PutSSHIdentity("a", clawssh.Identity{PrivateKeyPath: "/a", PublicKeyPath: "/a.pub"})
	_ = s.PutSSHIdentity("b", clawssh.Identity{PrivateKeyPath: "/b", PublicKeyPath: "/b.pub"})
	_ = s.UseSSHIdentity("b")
	if err := s.RemoveSSHIdentity("b"); err != nil {
		t.Fatalf("RemoveSSHIdentity: %v", err)
	}
	if s.SSHCurrent() != "a" {
		t.Fatalf("expected current switched to a, got %q", s.SSHCurrent())
	}
	if err := s.RemoveSSHIdentity("a"); err != nil {
		t.Fatalf("RemoveSSHIdentity: %v", err)
	}
	if s.SSHCurrent() != "" {
		t.Fatalf("expected current cleared, got %q", s.SSHCurrent())
	}
}

func TestStoreDeleteManifestClearsCurrent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	mp := filepath.Join(dir, "x.yml")
	_ = writeFile(mp, "prefix: x\n")

	s, err := Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetManifest("a", mp)
	_ = s.Use("a")
	if err := s.DeleteManifest("a"); err != nil {
		t.Fatal(err)
	}
	if s.CurrentManifestName() != "" {
		t.Fatalf("expected current cleared, got %q", s.CurrentManifestName())
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
