package state

import (
	"github.com/gluwa/openclaw-swarm2/internal/ssh"
)

// CurrentSchemaVersion is bumped when the on-disk YAML shape changes.
const CurrentSchemaVersion = 2

// Document is the persisted claws state (kubeconfig-style: manifests + auth).
type Document struct {
	Version   int                    `yaml:"version"`
	Manifests map[string]ManifestRef `yaml:"manifests"`
	Current   string                 `yaml:"current,omitempty"`
	Auth      Auth                   `yaml:"auth,omitempty"`
}

// ManifestRef points to a manifest file on disk (absolute path after SetManifest).
type ManifestRef struct {
	Path string `yaml:"path"`
}

// Auth holds authentication material separate from infrastructure manifests.
type Auth struct {
	SSH *SSHAuth `yaml:"ssh,omitempty"`
}

// SSHAuth holds named SSH identities and the active name (schema v2).
// Legacy v1 stored public_key_path / private_key_path directly under auth.ssh;
// Open migrates that to identities["default"].
type SSHAuth struct {
	Current    string                  `yaml:"current,omitempty"`
	Identities map[string]ssh.Identity `yaml:"identities,omitempty"`
	// v1 migration only (cleared after migrateSSHAuth)
	PublicKeyPath  string `yaml:"public_key_path,omitempty"`
	PrivateKeyPath string `yaml:"private_key_path,omitempty"`
}
