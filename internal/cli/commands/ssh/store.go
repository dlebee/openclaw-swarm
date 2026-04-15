package sshauth

import (
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/state"
)

func openStore() (*state.Store, error) {
	s, err := state.OpenDefault()
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	return s, nil
}
