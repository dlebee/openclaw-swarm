package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// ConfigureToolsStep ensures the agent's per-agent tools config (exec) and
// global tools.elevated config match the manifest. Uses `openclaw config set`.
type ConfigureToolsStep struct {
	dial   SSHDialFunc
	reader common.ConfigReader
}

func NewConfigureToolsStep(opts Options) *ConfigureToolsStep {
	return &ConfigureToolsStep{dial: opts.SSHDial, reader: opts.ConfigReader}
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
	host, ok := common.HostKnown(ctx, m)
	if !ok {
		return false, nil
	}
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer common.ReturnSSH(ctx, key, client)

	ch := common.MachineConfigHost(m, host)
	idx, err := s.reader.AgentConfigIndex(ctx, client, ch, at.Spec.ID)
	if err != nil {
		return false, fmt.Errorf("read agents.list on %s: %w", m.Name, err)
	}
	if idx < 0 {
		return false, nil
	}

	current, err := s.reader.AgentTools(ctx, client, ch, idx)
	if err != nil {
		return false, fmt.Errorf("read tools config on %s: %w", m.Name, err)
	}
	if !execMatch(current, at.Spec.Tools) {
		return false, nil
	}

	if at.Spec.Tools.Elevated != nil {
		elev, err := s.reader.Elevated(ctx, client, ch)
		if err != nil {
			return false, nil
		}
		if !elevatedMatch(elev, at.Spec.Tools.Elevated) {
			return false, nil
		}
	}
	return true, nil
}

func (s *ConfigureToolsStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	if at.Spec.Tools == nil {
		return nil
	}
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-tools: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	ch := common.MachineConfigHost(m, common.ResolveMachineHost(ctx, m))
	idx, err := s.reader.AgentConfigIndex(ctx, client, ch, at.Spec.ID)
	if err != nil || idx < 0 {
		return fmt.Errorf("configure-tools: agent %q not found in config", at.Spec.ID)
	}

	batch := buildExecBatch(idx, at.Spec.Tools)
	batch = append(batch, buildElevatedBatch(at.Spec.Tools.Elevated)...)
	if len(batch) == 0 {
		return nil
	}

	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("configure-tools: marshal: %w", err)
	}

	script := fmt.Sprintf(`set -euo pipefail
%sopenclaw config set --batch-json '%s'
`, common.OpenclawCLIPreamble(), string(batchJSON))

	// Serialize config mutations per machine to avoid ConfigMutationConflictError
	var out string
	err = common.WithConfigMutationLock(m.Name, func() error {
		var runErr error
		out, runErr = bash.RunOutput(client, script)
		return runErr
	})
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
	host := common.ResolveMachineHost(ctx, m)
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("configure-tools verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	ch := common.MachineConfigHost(m, host)
	idx, err := s.reader.AgentConfigIndex(ctx, client, ch, at.Spec.ID)
	if err != nil || idx < 0 {
		return fmt.Errorf("configure-tools verify: agent %q not found", at.Spec.ID)
	}

	current, err := s.reader.AgentTools(ctx, client, ch, idx)
	if err != nil {
		return fmt.Errorf("configure-tools verify: %w", err)
	}
	if !execMatch(current, at.Spec.Tools) {
		return fmt.Errorf("configure-tools verify: exec config drift")
	}
	if at.Spec.Tools.Elevated != nil {
		elev, err := s.reader.Elevated(ctx, client, ch)
		if err != nil {
			return fmt.Errorf("configure-tools verify: %w", err)
		}
		if !elevatedMatch(elev, at.Spec.Tools.Elevated) {
			return fmt.Errorf("configure-tools verify: elevated config drift")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers — per-agent exec
// ---------------------------------------------------------------------------

type batchEntry struct {
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func buildExecBatch(idx int, tools *manifestdata.AgentTools) []batchEntry {
	if tools == nil || tools.Exec == nil {
		return nil
	}
	var batch []batchEntry
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
	return batch
}

func execMatch(current *common.RemoteToolsConfig, desired *manifestdata.AgentTools) bool {
	if desired == nil || desired.Exec == nil {
		return true
	}
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
	return true
}

// ---------------------------------------------------------------------------
// helpers — global tools.elevated
// ---------------------------------------------------------------------------

func buildElevatedBatch(elev *manifestdata.AgentElevated) []batchEntry {
	if elev == nil {
		return nil
	}
	var batch []batchEntry
	if elev.Enabled != nil {
		batch = append(batch, batchEntry{"tools.elevated.enabled", *elev.Enabled})
	}
	for ch, ids := range elev.AllowFrom {
		batch = append(batch, batchEntry{
			fmt.Sprintf("tools.elevated.allowFrom.%s", ch), ids,
		})
	}
	return batch
}

func elevatedMatch(have *common.RemoteElevatedConfig, desired *manifestdata.AgentElevated) bool {
	if desired == nil {
		return true
	}
	if have == nil {
		have = &common.RemoteElevatedConfig{}
	}
	if desired.Enabled != nil {
		haveEnabled := have.Enabled != nil && *have.Enabled
		if haveEnabled != *desired.Enabled {
			return false
		}
	}
	for ch, wantIDs := range desired.AllowFrom {
		haveIDs := have.AllowFrom[ch]
		for _, wid := range wantIDs {
			found := false
			for _, hid := range haveIDs {
				if hid == wid {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}
