package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// ConfigureToolsStep ensures the agent's tools config (exec, elevated) matches
// the manifest. Drift-repairs via `openclaw config set --batch-json`.
type ConfigureToolsStep struct {
	dial SSHDialFunc
}

func NewConfigureToolsStep(opts Options) *ConfigureToolsStep {
	return &ConfigureToolsStep{dial: opts.SSHDial}
}

func (*ConfigureToolsStep) Name() string { return "configure-tools" }

func (s *ConfigureToolsStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	at, ok := t.Payload.(*AgentTarget)
	if !ok {
		return false, nil
	}
	return at.Spec.Tools != nil, nil
}

func (s *ConfigureToolsStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	at := t.Payload.(*AgentTarget)
	if at.Spec.Tools == nil {
		return true, nil
	}
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return false, nil
	}
	defer common.ReturnSSH(ctx, key, client)

	idx, err := AgentConfigIndex(client, at.Spec.ID)
	if err != nil || idx < 0 {
		return false, nil
	}

	current, err := readToolsConfig(client, idx)
	if err != nil {
		return false, nil
	}
	return toolsMatch(current, at.Spec.Tools), nil
}

func (s *ConfigureToolsStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	if at.Spec.Tools == nil {
		return nil
	}
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("configure-tools: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	idx, err := AgentConfigIndex(client, at.Spec.ID)
	if err != nil || idx < 0 {
		return fmt.Errorf("configure-tools: agent %q not found in config", at.Spec.ID)
	}

	batch := buildToolsBatch(idx, at.Spec.Tools)
	if len(batch) == 0 {
		return nil
	}

	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("configure-tools: marshal: %w", err)
	}

	script := fmt.Sprintf(`set -euo pipefail
openclaw config set --batch-json '%s'
`, string(batchJSON))

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("configure-tools: config set: %w\n%s", err, out)
	}
	return nil
}

func (s *ConfigureToolsStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	if at.Spec.Tools == nil {
		return nil
	}
	m := at.Machine
	client, key, err := common.BorrowSSH(ctx, s.dial, common.MachineHost(m), common.MachineSSHPort(m), common.MachineSSHUser(m))
	if err != nil {
		return fmt.Errorf("configure-tools verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	idx, err := AgentConfigIndex(client, at.Spec.ID)
	if err != nil || idx < 0 {
		return fmt.Errorf("configure-tools verify: agent %q not found", at.Spec.ID)
	}

	current, err := readToolsConfig(client, idx)
	if err != nil {
		return fmt.Errorf("configure-tools verify: %w", err)
	}
	if !toolsMatch(current, at.Spec.Tools) {
		return fmt.Errorf("configure-tools verify: tools config drift")
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type remoteExecConfig struct {
	Host     string `json:"host"`
	Node     string `json:"node"`
	Security string `json:"security"`
}

type remoteToolsConfig struct {
	Exec *remoteExecConfig `json:"exec"`
}

func readToolsConfig(client *xssh.Client, idx int) (*remoteToolsConfig, error) {
	key := fmt.Sprintf("agents.list[%d].tools", idx)
	out, err := bash.RunOutput(client, fmt.Sprintf(
		`openclaw config get %s --json 2>/dev/null || echo "{}"`, key))
	if err != nil {
		return nil, err
	}
	raw := extractJSON(strings.TrimSpace(out), '{')
	var cfg remoteToolsConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return &remoteToolsConfig{}, nil
	}
	return &cfg, nil
}


type batchEntry struct {
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func buildToolsBatch(idx int, tools *manifestdata.AgentTools) []batchEntry {
	var batch []batchEntry
	if tools.Exec != nil {
		prefix := fmt.Sprintf("agents.list[%d].tools.exec", idx)
		if tools.Exec.Host != "" {
			batch = append(batch, batchEntry{prefix + ".host", tools.Exec.Host})
		}
		if tools.Exec.Node != "" {
			batch = append(batch, batchEntry{prefix + ".node", tools.Exec.Node})
		}
		if tools.Exec.Security != "" {
			batch = append(batch, batchEntry{prefix + ".security", tools.Exec.Security})
		}
	}
	return batch
}

func toolsMatch(current *remoteToolsConfig, desired *manifestdata.AgentTools) bool {
	if desired == nil {
		return true
	}
	if desired.Exec != nil {
		if current == nil || current.Exec == nil {
			return false
		}
		if desired.Exec.Host != "" && current.Exec.Host != desired.Exec.Host {
			return false
		}
		if desired.Exec.Node != "" && current.Exec.Node != desired.Exec.Node {
			return false
		}
		if desired.Exec.Security != "" && current.Exec.Security != desired.Exec.Security {
			return false
		}
	}
	return true
}
