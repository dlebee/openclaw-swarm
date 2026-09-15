package common

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/apt"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// DefaultNodeMajor is the Node.js major line installed when the manifest
// leaves node_major unset. OpenClaw 2026.9.x requires
// `node >=24.16.0 <25 || >=26.1.0`, and 2026.7.x/2026.8.x accept 24.15+,
// so 24 covers every release the integration matrix exercises.
const DefaultNodeMajor = 24

// minNodeVersions is the oldest release per major line that OpenClaw's
// package.json `engines` accepts. A host on the wanted major but below the
// floor (e.g. installed from an older NodeSource snapshot) is treated as
// unsatisfied so Execute upgrades it in place. Majors without an entry have
// no floor beyond the major itself.
var minNodeVersions = map[int]NodeVersion{
	22: {22, 22, 3},
	24: {24, 16, 0},
	26: {26, 1, 0},
}

// NodeVersion is a parsed `node --version` output (e.g. "v24.21.0").
type NodeVersion struct {
	Major, Minor, Patch int
}

func (v NodeVersion) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v NodeVersion) less(o NodeVersion) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var nodeVersionPattern = regexp.MustCompile(`v(\d+)\.(\d+)\.(\d+)`)

// ParseNodeVersion extracts the first vMAJOR.MINOR.PATCH token from s.
func ParseNodeVersion(s string) (NodeVersion, error) {
	m := nodeVersionPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return NodeVersion{}, fmt.Errorf("not a node version: %q", strings.TrimSpace(s))
	}
	var v NodeVersion
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return NodeVersion{}, fmt.Errorf("node version %q: %w", m[0], err)
		}
		*dst = n
	}
	return v, nil
}

// nodeVersionSatisfies reports whether an installed version matches the
// wanted major line and clears that line's OpenClaw floor.
func nodeVersionSatisfies(have NodeVersion, wantMajor int) bool {
	if have.Major != wantMajor {
		return false
	}
	if floor, ok := minNodeVersions[wantMajor]; ok && have.less(floor) {
		return false
	}
	return true
}

// ResolveNodeMajor returns the manifest's node_major, or DefaultNodeMajor
// when it is unset.
func ResolveNodeMajor(manifestMajor int) int {
	if manifestMajor > 0 {
		return manifestMajor
	}
	return DefaultNodeMajor
}

// InstallNodejsStep ensures the wanted Node.js major line is installed on
// the target machine, upgrading hosts that carry a different major or a
// release below OpenClaw's floor for that major.
type InstallNodejsStep struct {
	dial         SSHDialFunc
	hostResolver HostResolverFn
	nodeMajor    int
}

func NewInstallNodejsStep(opts Options) *InstallNodejsStep {
	return &InstallNodejsStep{
		dial:         opts.SSHDial,
		hostResolver: opts.HostResolver,
		nodeMajor:    ResolveNodeMajor(opts.NodeMajor),
	}
}

func (*InstallNodejsStep) Name() string { return "install-nodejs" }

func (*InstallNodejsStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	_, ok := t.Payload.(MachineProvider)
	return ok, nil
}

func (s *InstallNodejsStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return false, nil
	}
	if s.dial == nil {
		return false, nil
	}
	m := mp.GetMachine()
	host, known := HostKnown(ctx, m, s.hostResolver)
	if !known {
		return false, nil
	}
	client, key, err := BorrowSSH(ctx, s.dial, host, MachineSSHPort(m), MachineAgentUser(m))
	if err != nil {
		return false, nil // connection failure — unsatisfied, Execute retries
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `node --version 2>/dev/null || echo missing`)
	if err != nil {
		return false, fmt.Errorf("probe node on %s: %w", m.Name, err)
	}
	have, err := ParseNodeVersion(out)
	if err != nil {
		return false, nil // not installed (or unparseable) — Execute installs
	}
	return nodeVersionSatisfies(have, s.nodeMajor), nil
}

func (s *InstallNodejsStep) Execute(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-nodejs: target %q does not provide a machine", t.ID)
	}
	if s.dial == nil {
		return fmt.Errorf("install-nodejs: SSH dialer not configured")
	}
	m := mp.GetMachine()
	host, port, user := ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineAgentUser(m)

	// Execute only runs when Check found node missing, on the wrong major,
	// or below the floor, so (re)running the NodeSource setup script is
	// always wanted: it points apt at the requested major line and refreshes
	// the package index, and the install then upgrades nodejs in place.
	script := fmt.Sprintf(`set -euo pipefail
curl -fsSL --http1.1 --retry 5 --retry-all-errors --retry-delay 3 https://deb.nodesource.com/setup_%d.x | sudo bash -
export DEBIAN_FRONTEND=noninteractive
sudo apt-get install -y -qq nodejs
`, s.nodeMajor)
	// apt.WithLockRetry retries the whole script on apt/dpkg lock
	// contention (apt-daily, unattended-upgrades). RunBashWithRetry
	// already handles transient SSH session drops; the two layers
	// target different failure classes.
	if err := apt.WithLockRetry(ctx, apt.RetryOpts{}, func() error {
		return RunBashWithRetry(ctx, s.dial, host, port, user, script)
	}); err != nil {
		return fmt.Errorf("install-nodejs: %w", err)
	}
	return nil
}

func (s *InstallNodejsStep) Verify(ctx context.Context, t scaffold.Target) error {
	mp, ok := t.Payload.(MachineProvider)
	if !ok {
		return fmt.Errorf("install-nodejs verify: target %q does not provide a machine", t.ID)
	}
	m := mp.GetMachine()
	// The nodesource setup script + apt install often trip needrestart,
	// which restarts sshd mid-session. Retry the dial so a single SYN
	// timeout doesn't fail the phase — same precedent as
	// InstallTailscaleStep.Verify.
	client, key, err := BorrowSSHWithRetry(ctx, s.dial, ResolveMachineHost(ctx, m), MachineSSHPort(m), MachineAgentUser(m))
	if err != nil {
		return fmt.Errorf("install-nodejs verify: dial: %w", err)
	}
	defer ReturnSSH(ctx, key, client)

	out, err := bash.RunOutput(client, `node --version`)
	if err != nil {
		return fmt.Errorf("install-nodejs verify: %w", err)
	}
	have, err := ParseNodeVersion(out)
	if err != nil {
		return fmt.Errorf("install-nodejs verify: %w", err)
	}
	if !nodeVersionSatisfies(have, s.nodeMajor) {
		// apt will not downgrade a newer major on its own; that case needs
		// an operator to remove nodejs (or set node_major to match).
		return fmt.Errorf("install-nodejs verify: node %s installed, want v%d.x%s",
			have, s.nodeMajor, floorSuffix(s.nodeMajor))
	}
	return nil
}

func floorSuffix(major int) string {
	if floor, ok := minNodeVersions[major]; ok {
		return " (>= " + floor.String() + ")"
	}
	return ""
}
