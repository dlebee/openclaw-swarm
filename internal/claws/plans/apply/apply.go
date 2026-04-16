// Package apply builds and runs the manifest infrastructure apply scaffold plan.
// Phases live under apply/<phase> (e.g. apply/provisioning).
package apply

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/agents"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/channels"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/mesh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/node"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/security"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

// BuildOptions is everything needed to assemble the apply plan (phases in order).
type BuildOptions struct {
	Manifest     *manifestdata.Manifest
	ManifestPath string // absolute path to the manifest file (for env_file resolution)
	Provider     hosting.Provider
	SSHPubKey    string
	// SSHDial is required when the manifest has any Linode machine (authorize-ssh-key step).
	SSHDial provisioning.SSHDialFunc
}

// BuildPlan returns a scaffold plan: phases are appended in apply order (provisioning first).
func BuildPlan(o BuildOptions) (*scaffold.Plan, error) {
	if o.Manifest == nil {
		return nil, fmt.Errorf("apply plan: manifest is nil")
	}
	if needsLinodeToken(o.Manifest.Machines) && o.SSHDial == nil {
		return nil, fmt.Errorf("apply plan: SSHDial is required when the manifest has Linode machines")
	}
	targets := provisioning.BuildMachineTargets(o.Manifest.Machines)

	p := scaffold.New()
	provisioning.AddPhase(p, targets, provisioning.Options{
		Provider:  o.Provider,
		Prefix:    o.Manifest.Prefix,
		SSHPubKey: o.SSHPubKey,
		SSHDial:   o.SSHDial,
	})
	security.AddPhase(p, targets, security.Options{
		SSHDial: security.SSHDialFunc(o.SSHDial),
	})
	mesh.AddPhase(p, targets, mesh.Options{
		SSHDial:  mesh.SSHDialFunc(o.SSHDial),
		Machines: o.Manifest.Machines,
		Gateways: o.Manifest.Gateways,
		Nodes:    o.Manifest.Nodes,
	})
	if len(o.Manifest.Gateways) > 0 {
		gwTargets := gateway.BuildGatewayTargets(o.Manifest.Gateways, o.Manifest.Machines)
		gateway.AddPhase(p, gwTargets, gateway.Options{
			SSHDial: gateway.SSHDialFunc(o.SSHDial),
		})
	}
	if hasChannels(o.Manifest.Gateways) {
		chTargets, err := channels.BuildChannelTargets(
			o.Manifest.Gateways, o.Manifest.Machines,
			func(envName string) (string, error) {
				return manifestsvc.LookupEnvFromManifest(o.ManifestPath, o.Manifest, envName)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("apply plan: resolve channel tokens: %w", err)
		}
		if len(chTargets) > 0 {
			channels.AddPhase(p, chTargets, channels.Options{
				SSHDial: channels.SSHDialFunc(o.SSHDial),
			})
		}
	}
	if len(o.Manifest.Nodes) > 0 {
		nodeTargets := node.BuildNodeTargets(o.Manifest.Nodes, o.Manifest.Machines, o.Manifest.Gateways)
		node.AddPhase(p, nodeTargets, node.Options{
			SSHDial: node.SSHDialFunc(o.SSHDial),
		})
	}
	if len(o.Manifest.Agents) > 0 {
		agentTargets := agents.BuildAgentTargets(o.Manifest.Agents, o.Manifest.Gateways, o.Manifest.Machines)
		agents.AddPhase(p, agentTargets, agents.Options{
			SSHDial: agents.SSHDialFunc(o.SSHDial),
		})
	}
	return p, nil
}

// LinodeProviderFromManifest returns a Linode client when the manifest has Linode machines;
// otherwise nil without error.
// manifestAbsPath is the absolute path to the manifest file (used to resolve relative env_file).
func LinodeProviderFromManifest(m *manifestdata.Manifest, manifestAbsPath string) (hosting.Provider, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	if !needsLinodeToken(m.Machines) {
		return nil, nil
	}
	if strings.TrimSpace(m.LinodeTokenEnv) == "" {
		return nil, fmt.Errorf("manifest linode_token_env is required when machines use type %q", manifestdata.MachineTypeLinode)
	}
	tok, err := manifestsvc.LookupEnvFromManifest(manifestAbsPath, m, m.LinodeTokenEnv)
	if err != nil {
		return nil, err
	}
	return linode.NewProvider(tok), nil
}

// RunOptions configures ExecWithConfirm for an apply run.
type RunOptions struct {
	DryRun      bool
	Out         io.Writer
	ProgressOut io.Writer
	Confirm     func() (bool, error)
	// PrettyPlan shows the plan in a Bubble Tea viewport (alternate screen) when Out is a TTY.
	PrettyPlan bool
}

// Run builds progress styling from ProgressOut, resolves width from Out when possible, then ExecWithConfirm.
func Run(ctx context.Context, plan *scaffold.Plan, o RunOptions) error {
	width := 80
	if f, ok := o.Out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w >= 40 {
			width = w
		}
	}
	progOut := o.ProgressOut
	if progOut == nil {
		progOut = io.Discard
	}
	styled := progress.NewStyled(progOut)

	var teaWriter io.Writer
	if f, ok := progOut.(*os.File); ok && !o.DryRun {
		if term.IsTerminal(int(f.Fd())) {
			teaWriter = f
		}
	}

	return scaffold.ExecWithConfirm(ctx, plan, scaffold.PipelineOptions{
		ExecuteOptions: scaffold.ExecuteOptions{
			DryRun:   o.DryRun,
			Progress: styled,
		},
		BuildProgress:     styled,
		Confirm:           o.Confirm,
		Width:             width,
		Out:               o.Out,
		PrettyPlan:        o.PrettyPlan,
		TeaProgressWriter: teaWriter,
	})
}

func needsLinodeToken(machines []manifestdata.Machine) bool {
	for _, m := range machines {
		if m.Type == manifestdata.MachineTypeLinode {
			return true
		}
	}
	return false
}

func hasChannels(gateways []manifestdata.Gateway) bool {
	for _, gw := range gateways {
		if len(gw.Channels) > 0 {
			return true
		}
	}
	return false
}
