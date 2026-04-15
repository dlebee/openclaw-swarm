// Package apply builds and runs the manifest infrastructure apply scaffold plan.
// Phases live under apply/<phase> (e.g. apply/provisioning).
package apply

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	"golang.org/x/term"
)

// BuildOptions is everything needed to assemble the apply plan (phases in order).
type BuildOptions struct {
	Manifest  *manifestdata.Manifest
	Provider  hosting.Provider
	SSHPubKey string
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
	p := scaffold.New()
	provisioning.AddPhase(p, o.Manifest.Machines, provisioning.Options{
		Provider:  o.Provider,
		Prefix:    o.Manifest.Prefix,
		SSHPubKey: o.SSHPubKey,
		SSHDial:   o.SSHDial,
	})
	return p, nil
}

// LinodeProviderFromManifest returns a Linode client when the manifest has Linode machines;
// otherwise nil without error.
func LinodeProviderFromManifest(m *manifestdata.Manifest) (hosting.Provider, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	if !needsLinodeToken(m.Machines) {
		return nil, nil
	}
	if strings.TrimSpace(m.LinodeTokenEnv) == "" {
		return nil, fmt.Errorf("manifest linode_token_env is required when machines use type %q", manifestdata.MachineTypeLinode)
	}
	tok := strings.TrimSpace(os.Getenv(m.LinodeTokenEnv))
	if tok == "" {
		return nil, fmt.Errorf("environment variable %q is not set (Linode API token)", m.LinodeTokenEnv)
	}
	return linode.NewProvider(tok), nil
}

// RunOptions configures ExecWithConfirm for an apply run.
type RunOptions struct {
	DryRun      bool
	Out         io.Writer
	ProgressOut io.Writer
	Confirm     func() (bool, error)
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
	return scaffold.ExecWithConfirm(ctx, plan, scaffold.PipelineOptions{
		ExecuteOptions: scaffold.ExecuteOptions{
			DryRun:   o.DryRun,
			Progress: styled,
		},
		BuildProgress: styled,
		Confirm:       o.Confirm,
		Width:         width,
		Out:           o.Out,
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
