package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/joho/godotenv"
)

// LookupEnvFromManifest resolves an environment variable name using the process
// environment first, then the manifest's env_file (relative to the manifest YAML path).
func LookupEnvFromManifest(manifestAbsPath string, m *data.Manifest, name string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("manifest is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("environment variable name is empty")
	}
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, nil
	}
	rel := strings.TrimSpace(m.EnvFile)
	if rel == "" {
		return "", fmt.Errorf("environment variable %q is not set (manifest has no env_file)", name)
	}
	dir := filepath.Dir(manifestAbsPath)
	envPath := filepath.Join(dir, filepath.FromSlash(rel))
	vars, err := godotenv.Read(envPath)
	if err != nil {
		return "", fmt.Errorf("read manifest env_file %q: %w", rel, err)
	}
	if v := strings.TrimSpace(vars[name]); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %q is not set in process environment or in env_file %q", name, rel)
}
