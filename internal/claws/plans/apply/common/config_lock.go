package common

import (
	"sync"
)

// configMutationLocks provides per-machine locks for OpenClaw config mutations.
// OpenClaw's CLI detects concurrent config writes (ConfigMutationConflictError)
// but doesn't auto-retry, so we serialize config-mutating commands per machine
// to avoid conflicts when multiple agents are created in parallel.
//
// This only locks config-mutating commands (config set, agents add, etc.),
// not entire agent setup tasks - so SSH, file copies, and workspace setup
// still run in parallel.
var configMutationLocks = struct {
	sync.Mutex
	m map[string]*sync.Mutex
}{
	m: make(map[string]*sync.Mutex),
}

// WithConfigMutationLock serializes config-mutating commands for a machine.
// Use this wrapper around any bash.RunOutput that calls `openclaw config set`,
// `openclaw agents add`, or other config-mutating CLI commands.
func WithConfigMutationLock(machineKey string, fn func() error) error {
	lock := getConfigMutationLock(machineKey)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func getConfigMutationLock(machineKey string) *sync.Mutex {
	configMutationLocks.Lock()
	defer configMutationLocks.Unlock()
	if configMutationLocks.m[machineKey] == nil {
		configMutationLocks.m[machineKey] = &sync.Mutex{}
	}
	return configMutationLocks.m[machineKey]
}
