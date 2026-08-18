package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// EnsureModelStep ensures the agent's model config (primary + fallbacks)
// and its per-model runtime pins match the manifest. Drift-repairs via
// `openclaw config set --batch-json`.
type EnsureModelStep struct {
	dial   SSHDialFunc
	reader common.ConfigReader
}

func NewEnsureModelStep(opts Options) *EnsureModelStep {
	return &EnsureModelStep{dial: opts.SSHDial, reader: opts.ConfigReader}
}

func (*EnsureModelStep) Name() string { return "ensure-model" }

func (s *EnsureModelStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(*AgentTarget)
	return ok, nil
}

func (s *EnsureModelStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	at := t.Payload.(*AgentTarget)
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

	primary, fallbacks, exists, err := s.reader.AgentModelFull(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
	if err != nil {
		return false, fmt.Errorf("find agent %q: %w", at.Spec.ID, err)
	}
	if !exists {
		return false, nil
	}
	// Check primary model
	if primary != at.Spec.Model.Primary {
		return false, nil
	}
	// Check fallbacks match
	if !stringSlicesEqual(fallbacks, at.Spec.Model.Fallbacks) {
		return false, nil
	}
	// Check per-model runtime pins match
	if len(at.Spec.Models) > 0 {
		entries, entriesExist, err := s.reader.AgentModelEntries(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
		if err != nil {
			return false, fmt.Errorf("read model entries for agent %q: %w", at.Spec.ID, err)
		}
		if !entriesExist || len(modelRuntimeDrift(at.Spec.Models, entries)) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// modelRuntimeDrift reports every manifest-declared model ref whose remote
// `agentRuntime.id` differs from the manifest. Refs present remotely but
// absent from the manifest are ignored: claws only owns what the manifest
// declares, and an agent's models map legitimately carries entries written
// by openclaw itself or by an automation.
func modelRuntimeDrift(desired manifestdata.AgentModels, remote map[string]map[string]any) []string {
	var drifted []string
	for ref, entry := range desired {
		want := strings.TrimSpace(entry.Runtime)
		if want == "" {
			continue
		}
		if common.ModelRuntimeID(remote[ref]) != want {
			drifted = append(drifted, ref)
		}
	}
	sort.Strings(drifted)
	return drifted
}

// stringSlicesEqual compares two string slices for equality.
// nil and empty slice are considered equal.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *EnsureModelStep) Execute(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("ensure-model: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	idx, err := s.reader.AgentConfigIndex(ctx, client, common.MachineConfigHost(m, common.ResolveMachineHost(ctx, m)), at.Spec.ID)
	if err != nil || idx < 0 {
		return fmt.Errorf("ensure-model: agent %q not found in config", at.Spec.ID)
	}

	type batchEntry struct {
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}

	// openclaw's schema (src/config/types.agents.ts) accepts
	// `agents.list[].model` as either a bare string OR the object form
	// `{primary, fallbacks}` — fallbacks lives INSIDE model, never as a
	// sibling on the agent entry. Writing `agents.list[].fallbacks`
	// would be rejected with "Unrecognized key: fallbacks". We therefore
	// collapse to a single batch entry: object form when fallbacks are
	// set, string form otherwise. One atomic write also sidesteps the
	// intermediate state where a scalar `model` would need promoting to
	// an object before a sibling `model.fallbacks` write could land.
	modelPath := fmt.Sprintf("agents.list[%d].model", idx)
	var modelValue interface{} = at.Spec.Model.Primary
	if len(at.Spec.Model.Fallbacks) > 0 {
		modelValue = map[string]interface{}{
			"primary":   at.Spec.Model.Primary,
			"fallbacks": at.Spec.Model.Fallbacks,
		}
	}
	batch := []batchEntry{{Path: modelPath, Value: modelValue}}

	// Per-model runtime pins live in a sibling map, not inside `model` —
	// openclaw keys agent-runtime policy by model ref
	// (`agents.list[].models[<ref>].agentRuntime.id`), because the runtime
	// that executes a turn is a property of the model, not of the agent.
	//
	// The whole map is rewritten in one value rather than one dotted path
	// per ref: model refs contain "/" and may contain "." (e.g. gpt-4.1),
	// which a dotted config path cannot express. Merging on top of what
	// the gateway already has keeps refs and sibling keys claws does not
	// own (aliases, thinking policy, …).
	if len(at.Spec.Models) > 0 {
		host := common.MachineConfigHost(m, common.ResolveMachineHost(ctx, m))
		existing, _, err := s.reader.AgentModelEntries(ctx, client, host, at.Spec.ID)
		if err != nil {
			return fmt.Errorf("ensure-model: read model entries: %w", err)
		}
		merged := make(map[string]any, len(existing)+len(at.Spec.Models))
		for ref, entry := range existing {
			merged[ref] = entry
		}
		for ref, entry := range at.Spec.Models {
			runtime := strings.TrimSpace(entry.Runtime)
			if runtime == "" {
				continue
			}
			merged[ref] = common.WithModelRuntimeID(existing[ref], runtime)
		}
		batch = append(batch, batchEntry{
			Path:  fmt.Sprintf("agents.list[%d].models", idx),
			Value: merged,
		})
	}

	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("ensure-model: marshal: %w", err)
	}

	script := fmt.Sprintf(`set -euo pipefail
%sopenclaw config set --batch-json '%s'
`, common.OpenclawCLIPreamble(), string(batchJSON))

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("ensure-model: config set: %w\n%s", err, out)
	}
	return nil
}

func (s *EnsureModelStep) Verify(ctx context.Context, t scaffold.Target) error {
	at := t.Payload.(*AgentTarget)
	m := at.Machine
	host := common.ResolveMachineHost(ctx, m)
	client, key, err := common.BorrowSSH(ctx, s.dial, host, common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("ensure-model verify: dial: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	primary, fallbacks, exists, err := s.reader.AgentModelFull(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
	if err != nil {
		return fmt.Errorf("ensure-model verify: %w", err)
	}
	if !exists {
		return fmt.Errorf("ensure-model verify: agent %q not found", at.Spec.ID)
	}
	if primary != at.Spec.Model.Primary {
		return fmt.Errorf("ensure-model verify: primary model %q, want %q", primary, at.Spec.Model.Primary)
	}
	if !stringSlicesEqual(fallbacks, at.Spec.Model.Fallbacks) {
		return fmt.Errorf("ensure-model verify: fallbacks %v, want %v", fallbacks, at.Spec.Model.Fallbacks)
	}
	if len(at.Spec.Models) > 0 {
		entries, entriesExist, err := s.reader.AgentModelEntries(ctx, client, common.MachineConfigHost(m, host), at.Spec.ID)
		if err != nil {
			return fmt.Errorf("ensure-model verify: %w", err)
		}
		if !entriesExist {
			return fmt.Errorf("ensure-model verify: agent %q not found", at.Spec.ID)
		}
		if drifted := modelRuntimeDrift(at.Spec.Models, entries); len(drifted) > 0 {
			ref := drifted[0]
			return fmt.Errorf(
				"ensure-model verify: model %q runtime %q, want %q",
				ref, common.ModelRuntimeID(entries[ref]), at.Spec.Models[ref].Runtime,
			)
		}
	}
	return nil
}
