package service

import (
	"fmt"
	"os"

	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"gopkg.in/yaml.v3"
)

// LoadFile reads a YAML manifest from path and decodes it into a Manifest.
func LoadFile(path string) (*data.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m data.Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &m, nil
}
