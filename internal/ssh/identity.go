package ssh

// Identity is persisted SSH key material as filesystem paths (PEM private + public).
type Identity struct {
	PublicKeyPath  string `yaml:"public_key_path"`
	PrivateKeyPath string `yaml:"private_key_path"`
}
