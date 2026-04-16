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
	envPath := resolveEnvFilePath(manifestAbsPath, rel)
	vars, err := godotenv.Read(envPath)
	if err != nil {
		return "", fmt.Errorf("read manifest env_file %q: %w", rel, err)
	}
	if v := strings.TrimSpace(vars[name]); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %q is not set in process environment or in env_file %q", name, rel)
}

// LoadEnvFile returns the merged environment map for a manifest: every entry
// from the manifest's env_file (relative to manifestAbsPath) overlaid with the
// current process env. Process values win over file values — same precedence as
// LookupEnvFromManifest. Empty values are pruned so callers can detect
// "present" via a simple `v, ok := map[name]` check.
//
// A nil manifest or an empty env_file returns an empty map without error, so
// callers can unconditionally call this and treat a missing file as "no vars".
// A present-but-unreadable env_file is an error (typo > silent miss).
func LoadEnvFile(manifestAbsPath string, m *data.Manifest) (map[string]string, error) {
	out := make(map[string]string)
	if m != nil {
		if rel := strings.TrimSpace(m.EnvFile); rel != "" {
			envPath := resolveEnvFilePath(manifestAbsPath, rel)
			fileVars, err := godotenv.Read(envPath)
			if err != nil {
				return nil, fmt.Errorf("read manifest env_file %q: %w", rel, err)
			}
			for k, v := range fileVars {
				if v = strings.TrimSpace(v); v != "" {
					out[k] = v
				}
			}
		}
	}
	// Process env wins; only overlay keys that are actually set so we don't
	// pull in the entire host environment.
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		v := strings.TrimSpace(kv[eq+1:])
		if v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// resolveEnvFilePath joins a manifest-relative env_file path against the
// directory of the manifest YAML. Absolute paths pass through unchanged.
func resolveEnvFilePath(manifestAbsPath, rel string) string {
	p := filepath.FromSlash(rel)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(manifestAbsPath), p)
}
