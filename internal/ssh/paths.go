package ssh

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath replaces a leading "~" or "~/" with the home directory.
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return filepath.Clean(p), nil
}

// DefaultKeysRoot returns the directory for Claws-managed SSH keys, e.g.
// ~/.config/claws/ssh/keys (Unix) or %APPDATA%\claws\ssh\keys (Windows).
func DefaultKeysRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claws", "ssh", "keys"), nil
}

// IdentityDir returns the directory holding key.pem and key.pub for a named identity.
func IdentityDir(keysRoot, name string) string {
	return filepath.Join(keysRoot, name)
}
