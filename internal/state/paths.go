package state

import (
	"os"
	"path/filepath"
)

// DefaultConfigPath returns the default claws state file (kubeconfig-like), e.g.
// ~/.config/claws/config.yaml (Unix) or %APPDATA%\claws\config.yaml (Windows).
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claws", "config.yaml"), nil
}
