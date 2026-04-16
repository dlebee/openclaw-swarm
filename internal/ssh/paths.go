package ssh

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ExpandPath replaces a leading "~" with the user home directory when followed by
// a path separator: "~/" everywhere, and "~\" on Windows (Claws stores Windows paths that way).
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		return home, nil
	}
	if len(p) >= 2 && p[0] == '~' {
		sep := p[1]
		if sep == '/' || (sep == '\\' && runtime.GOOS == "windows") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("user home: %w", err)
			}
			return filepath.Join(home, p[2:]), nil
		}
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
