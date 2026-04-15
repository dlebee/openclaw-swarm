package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	privateKeyFile = "key.pem"
	publicKeyFile  = "key.pub"
)

// ValidIdentityName returns an error if name is not a safe identifier.
func ValidIdentityName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("identity name is empty")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("identity name %q: use only letters, digits, hyphen, underscore", name)
		}
	}
	return nil
}

// GeneratePEMIdentity creates a new Ed25519 key pair, writes PEM private key and
// OpenSSH public key line to keysRoot/name/, and returns the identity paths.
func GeneratePEMIdentity(keysRoot, name string) (Identity, error) {
	if err := ValidIdentityName(name); err != nil {
		return Identity{}, err
	}
	dir := IdentityDir(keysRoot, name)
	if _, err := os.Stat(dir); err == nil {
		return Identity{}, fmt.Errorf("identity %q already exists at %s", name, dir)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	privPEM, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return Identity{}, err
	}
	pemData := pem.EncodeToMemory(privPEM)

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return Identity{}, err
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + "\n"

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, fmt.Errorf("mkdir identity dir: %w", err)
	}
	privPath := filepath.Join(dir, privateKeyFile)
	pubPath := filepath.Join(dir, publicKeyFile)
	if err := os.WriteFile(privPath, pemData, 0o600); err != nil {
		return Identity{}, fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(pubLine), 0o644); err != nil {
		return Identity{}, fmt.Errorf("write public key: %w", err)
	}

	privDisp, err := displayPath(privPath)
	if err != nil {
		privDisp = privPath
	}
	pubDisp, err := displayPath(pubPath)
	if err != nil {
		pubDisp = pubPath
	}

	return Identity{
		PrivateKeyPath: privDisp,
		PublicKeyPath:  pubDisp,
	}, nil
}

func displayPath(abs string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return abs, err
	}
	home = filepath.Clean(home)
	abs = filepath.Clean(abs)
	sep := string(filepath.Separator)
	if strings.HasPrefix(abs, home+sep) || abs == home {
		rel, err := filepath.Rel(home, abs)
		if err != nil {
			return abs, nil
		}
		return "~" + sep + rel, nil
	}
	return abs, nil
}
