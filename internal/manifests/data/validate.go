package data

import (
	"fmt"
	"strconv"
	"strings"
)

// reservedMainAgentID is the openclaw default agent id. Its workspace is
// bootstrapped by the gateway phase before the agents phase runs, so
// overriding it via the manifest produces an incomplete second workspace.
const reservedMainAgentID = "main"

// Validate runs a best-effort static check of the manifest that the YAML
// parser can't enforce on its own. It's intentionally narrow — we only gate
// the things that are genuinely unsafe to discover at execute time:
//
//   - References to the reserved "self" target without AllowSelf opt-in.
//   - scp.* steps missing source/destination, using self on the target side
//     (both sides would be self), or carrying an invalid mode.
//   - A real machine entry literally named "self" (shadows the reserved
//     target).
//   - Unknown step kinds (easier to catch here than at dispatch time with
//     "no matching case" silently skipping work).
//   - A workspace override on the reserved "main" agent (the gateway phase
//     bootstraps the default workspace before the agents phase runs; setting
//     a different path here produces an incomplete second workspace while the
//     bootstrap files remain in the original location).
//
// Everything else (machine references in automations, duplicate names, etc.)
// is left to the phase builders, which already emit clear errors and whose
// tolerance varies by scenario (e.g. `claws automations apply` legitimately
// ignores machines that don't exist).
func ValidateManifest(m *Manifest) error {
	if m == nil {
		return nil
	}

	for _, mach := range m.Machines {
		if strings.EqualFold(strings.TrimSpace(mach.Name), SelfMachineName) {
			return fmt.Errorf("machine name %q is reserved for the local-execution target; rename the machine", mach.Name)
		}
	}

	for _, agent := range m.Agents {
		if strings.EqualFold(strings.TrimSpace(agent.ID), reservedMainAgentID) &&
			strings.TrimSpace(agent.Workspace) != "" {
			return fmt.Errorf(
				"agent %q: workspace override is not supported for the reserved \"main\" agent — "+
					"the gateway bootstrap creates the default workspace before the agents phase runs, "+
					"so setting a custom path here produces an incomplete second workspace; "+
					"remove the workspace field to use the openclaw default (~/.openclaw/workspace)",
				agent.ID,
			)
		}
	}

	if err := validateNodeGatewayColocation(m); err != nil {
		return err
	}

	for _, auto := range m.Automations {
		if err := validateAutomation(auto, m.AllowSelf); err != nil {
			return fmt.Errorf("automation %q: %w", auto.Name, err)
		}
	}
	return nil
}

// validateNodeGatewayColocation rejects any node whose reference machine is
// the same as its gateway's reference machine. Colocating a node and gateway
// on the same host creates a circular dependency in the pairing protocol:
// the node registers with the gateway over the local network, but the gateway
// process is not yet paired and ready when the node first starts. Keep them
// on separate machines, or remove the node entry entirely if exec is not needed.
func validateNodeGatewayColocation(m *Manifest) error {
	gwRefByName := make(map[string]string, len(m.Gateways))
	for _, gw := range m.Gateways {
		gwRefByName[gw.Name] = strings.TrimSpace(gw.Reference)
	}
	for _, n := range m.Nodes {
		nodeRef := strings.TrimSpace(n.Reference)
		gwRef, ok := gwRefByName[strings.TrimSpace(n.Gateway)]
		if !ok {
			continue // unknown gateway reference — let the phase builder catch it
		}
		if nodeRef != "" && nodeRef == gwRef {
			return fmt.Errorf(
				"node %q and its gateway %q both reference machine %q; "+
					"nodes and gateways must run on separate machines",
				n.Name, n.Gateway, nodeRef,
			)
		}
	}
	return nil
}

func validateAutomation(auto Automation, allowSelf bool) error {
	touchesSelf := false
	remoteCount := 0
	for _, n := range auto.Machines {
		if IsSelfMachineName(n) {
			touchesSelf = true
		} else {
			remoteCount++
		}
	}
	if touchesSelf && !allowSelf {
		return fmt.Errorf("references reserved target %q but manifest does not set allow_self: true", SelfMachineName)
	}

	for i, step := range auto.Steps {
		if err := validateStep(step, touchesSelf, remoteCount, allowSelf); err != nil {
			ref := step.Name
			if ref == "" {
				ref = fmt.Sprintf("#%d", i)
			}
			return fmt.Errorf("step %s: %w", ref, err)
		}
	}
	return nil
}

func validateStep(step AutomationStep, autoTouchesSelf bool, remoteCount int, allowSelf bool) error {
	kind := strings.ToLower(strings.TrimSpace(step.Kind))
	if kind == "" {
		kind = StepKindBash
	}
	switch kind {
	case StepKindBash, StepKindPython:
		if step.IfChanged {
			return fmt.Errorf("if_changed is only valid on kind: scp.upload, not %q", kind)
		}
		return nil
	case StepKindSCPUpload, StepKindSCPDownload:
		if !allowSelf {
			return fmt.Errorf("kind %q requires manifest.allow_self: true (self is always one side of the transfer)", kind)
		}
		if strings.TrimSpace(step.Source) == "" {
			return fmt.Errorf("kind %q requires a non-empty `source`", kind)
		}
		if strings.TrimSpace(step.Destination) == "" {
			return fmt.Errorf("kind %q requires a non-empty `destination`", kind)
		}
		if strings.TrimSpace(step.Execute) != "" || strings.TrimSpace(step.ExecuteFile) != "" {
			return fmt.Errorf("kind %q does not take an `execute` script; use `source`/`destination` instead", kind)
		}
		if remoteCount == 0 {
			return fmt.Errorf("kind %q requires at least one remote machine in `machines:` (self is the implicit other side)", kind)
		}
		if autoTouchesSelf {
			return fmt.Errorf("kind %q automations must NOT list self in `machines:` — self is implicit (the %s side)", kind, scpSelfSide(kind))
		}
		if err := validateMode(step.Mode); err != nil {
			return err
		}
		if step.IfChanged && kind != StepKindSCPUpload {
			return fmt.Errorf("if_changed is only valid on kind: scp.upload, not %q", kind)
		}
		return nil
	default:
		return fmt.Errorf("unknown kind %q (supported: bash, python, scp.upload, scp.download)", step.Kind)
	}
}

func scpSelfSide(kind string) string {
	if kind == StepKindSCPUpload {
		return "source"
	}
	return "destination"
}

func validateMode(mode string) error {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return nil
	}
	// Accept with or without 0 prefix; must be a 3- or 4-digit octal.
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return fmt.Errorf("mode %q is not a valid octal permission string (e.g. 0755)", mode)
	}
	if v > 07777 {
		return fmt.Errorf("mode %q is out of range (must fit in 4 octal digits)", mode)
	}
	return nil
}

// IsSelfMachineName reports whether name refers to the reserved "self"
// target. Comparison is case-insensitive and whitespace-trimmed so operators
// can spell it however they like in YAML.
func IsSelfMachineName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), SelfMachineName)
}

// ParseMode parses a validated mode string into a uint32 suitable for
// passing to os.Chmod / sftp.Chmod. Returns (0, false) for an empty string
// so callers can distinguish "no mode set" from "mode 0".
func ParseMode(mode string) (uint32, bool) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}
