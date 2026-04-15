package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// Store reads and writes a Document to a single YAML file.
type Store struct {
	path string
	doc  Document
}

// Open loads path or returns an empty document if the file is missing.
func Open(path string) (*Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{
				path: path,
				doc: Document{
					Version:   CurrentSchemaVersion,
					Manifests: map[string]ManifestRef{},
				},
			}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var doc Document
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if doc.Version == 0 {
		doc.Version = CurrentSchemaVersion
	}
	if doc.Manifests == nil {
		doc.Manifests = map[string]ManifestRef{}
	}
	migrateSSHAuth(&doc.Auth)
	doc.Version = CurrentSchemaVersion
	return &Store{path: path, doc: doc}, nil
}

func migrateSSHAuth(auth *Auth) {
	if auth == nil || auth.SSH == nil {
		return
	}
	s := auth.SSH
	if len(s.Identities) > 0 {
		return
	}
	if s.PrivateKeyPath == "" {
		return
	}
	if s.Identities == nil {
		s.Identities = map[string]clawssh.Identity{}
	}
	s.Identities["default"] = clawssh.Identity{
		PublicKeyPath:  s.PublicKeyPath,
		PrivateKeyPath: s.PrivateKeyPath,
	}
	if s.Current == "" {
		s.Current = "default"
	}
	s.PublicKeyPath = ""
	s.PrivateKeyPath = ""
}

// OpenDefault opens DefaultConfigPath().
func OpenDefault() (*Store, error) {
	p, err := DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	return Open(p)
}

// Path is the backing file path.
func (s *Store) Path() string { return s.path }

// Document returns a copy of the persisted document.
func (s *Store) Document() Document {
	return s.doc
}

// Save writes the store to disk with restrictive permissions.
func (s *Store) Save() error {
	s.doc.Version = CurrentSchemaVersion
	migrateSSHAuth(&s.doc.Auth)
	b, err := yaml.Marshal(&s.doc)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write state temp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// SetManifest registers or updates a named manifest path (stored absolute).
func (s *Store) SetManifest(name, manifestPath string) error {
	if name == "" {
		return fmt.Errorf("manifest name is empty")
	}
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return fmt.Errorf("manifest path: %w", err)
	}
	if s.doc.Manifests == nil {
		s.doc.Manifests = map[string]ManifestRef{}
	}
	s.doc.Manifests[name] = ManifestRef{Path: abs}
	return nil
}

// DeleteManifest removes a named manifest. If it was current, Current is cleared.
func (s *Store) DeleteManifest(name string) error {
	if name == "" {
		return fmt.Errorf("manifest name is empty")
	}
	delete(s.doc.Manifests, name)
	if s.doc.Current == name {
		s.doc.Current = ""
	}
	return nil
}

// ListManifests returns registered names and paths (order not guaranteed).
func (s *Store) ListManifests() map[string]ManifestRef {
	out := make(map[string]ManifestRef, len(s.doc.Manifests))
	for k, v := range s.doc.Manifests {
		out[k] = v
	}
	return out
}

// Use sets the active manifest name.
func (s *Store) Use(name string) error {
	if name == "" {
		return fmt.Errorf("manifest name is empty")
	}
	if _, ok := s.doc.Manifests[name]; !ok {
		return fmt.Errorf("unknown manifest %q", name)
	}
	s.doc.Current = name
	return nil
}

// CurrentManifestName returns the active name, or empty if none.
func (s *Store) CurrentManifestName() string {
	return s.doc.Current
}

// CurrentManifestPath returns the filesystem path for the active manifest.
func (s *Store) CurrentManifestPath() (string, error) {
	if s.doc.Current == "" {
		return "", fmt.Errorf("no current manifest")
	}
	ref, ok := s.doc.Manifests[s.doc.Current]
	if !ok {
		return "", fmt.Errorf("current manifest %q not found in registry", s.doc.Current)
	}
	return ref.Path, nil
}

// PutSSHIdentity registers or replaces a named SSH identity.
func (s *Store) PutSSHIdentity(name string, id clawssh.Identity) error {
	if err := clawssh.ValidIdentityName(name); err != nil {
		return err
	}
	if id.PrivateKeyPath == "" {
		return fmt.Errorf("identity %q: private_key_path is empty", name)
	}
	if s.doc.Auth.SSH == nil {
		s.doc.Auth.SSH = &SSHAuth{Identities: map[string]clawssh.Identity{}}
	}
	if s.doc.Auth.SSH.Identities == nil {
		s.doc.Auth.SSH.Identities = map[string]clawssh.Identity{}
	}
	cp := id
	s.doc.Auth.SSH.Identities[name] = cp
	return nil
}

// RemoveSSHIdentity deletes a named identity. If it was active and other identities
// remain, current is switched to the first remaining name (sorted); otherwise current is cleared.
func (s *Store) RemoveSSHIdentity(name string) error {
	if err := clawssh.ValidIdentityName(name); err != nil {
		return err
	}
	if s.doc.Auth.SSH == nil || s.doc.Auth.SSH.Identities == nil {
		return fmt.Errorf("unknown ssh identity %q", name)
	}
	if _, ok := s.doc.Auth.SSH.Identities[name]; !ok {
		return fmt.Errorf("unknown ssh identity %q", name)
	}
	wasCurrent := s.doc.Auth.SSH.Current == name
	delete(s.doc.Auth.SSH.Identities, name)
	if wasCurrent {
		if len(s.doc.Auth.SSH.Identities) == 0 {
			s.doc.Auth.SSH.Current = ""
		} else {
			names := make([]string, 0, len(s.doc.Auth.SSH.Identities))
			for n := range s.doc.Auth.SSH.Identities {
				names = append(names, n)
			}
			sort.Strings(names)
			s.doc.Auth.SSH.Current = names[0]
		}
	}
	return nil
}

// UseSSHIdentity sets the active SSH identity name.
func (s *Store) UseSSHIdentity(name string) error {
	if err := clawssh.ValidIdentityName(name); err != nil {
		return err
	}
	if s.doc.Auth.SSH == nil || s.doc.Auth.SSH.Identities == nil {
		return fmt.Errorf("unknown ssh identity %q", name)
	}
	if _, ok := s.doc.Auth.SSH.Identities[name]; !ok {
		return fmt.Errorf("unknown ssh identity %q", name)
	}
	s.doc.Auth.SSH.Current = name
	return nil
}

// SSHCurrent returns the active SSH identity name, or empty if none.
func (s *Store) SSHCurrent() string {
	if s.doc.Auth.SSH == nil {
		return ""
	}
	return s.doc.Auth.SSH.Current
}

// SSHIdentities returns a copy of registered SSH identities.
func (s *Store) SSHIdentities() map[string]clawssh.Identity {
	if s.doc.Auth.SSH == nil || s.doc.Auth.SSH.Identities == nil {
		return nil
	}
	out := make(map[string]clawssh.Identity, len(s.doc.Auth.SSH.Identities))
	for k, v := range s.doc.Auth.SSH.Identities {
		out[k] = v
	}
	return out
}

// ActiveSSHIdentity returns the resolved identity for the current name.
func (s *Store) ActiveSSHIdentity() (*clawssh.Identity, error) {
	if s.doc.Auth.SSH == nil || s.doc.Auth.SSH.Current == "" {
		return nil, fmt.Errorf("no ssh identity configured (claws auth generate, claws auth use)")
	}
	id, ok := s.doc.Auth.SSH.Identities[s.doc.Auth.SSH.Current]
	if !ok {
		return nil, fmt.Errorf("current ssh identity %q is missing from state", s.doc.Auth.SSH.Current)
	}
	cp := id
	return &cp, nil
}

// LoadCurrentManifest loads the active manifest from disk.
func (s *Store) LoadCurrentManifest() (*manifestdata.Manifest, error) {
	p, err := s.CurrentManifestPath()
	if err != nil {
		return nil, err
	}
	return manifestsvc.LoadFile(p)
}

// DialSSH opens an SSH client using persisted auth. addr is host or host:port.
func (s *Store) DialSSH(addr, user string) (*xssh.Client, error) {
	id, err := s.ActiveSSHIdentity()
	if err != nil {
		return nil, err
	}
	return clawssh.Dial("tcp", addr, id, user)
}
