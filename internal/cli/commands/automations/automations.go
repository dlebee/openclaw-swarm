package automationscmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	planaut "github.com/gluwa/openclaw-swarm2/internal/claws/plans/automations"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
)

// AutomationsCmd returns the `claws automations` command group.
func AutomationsCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "automations",
		Short: "Run user-defined automation phases",
	}
	cmd.AddCommand(applyCmd(manifestFile))
	return cmd
}

func applyCmd(manifestFile *string) *cobra.Command {
	var (
		dryRun     bool
		prettyPlan bool
		names      []string
	)
	cmd := &cobra.Command{
		Use:   "apply [manifest.yml]",
		Short: "Run automation phases from the manifest",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveManifestPath(manifestFile, args)
			if err != nil {
				return err
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("manifest path: %w", err)
			}
			m, err := manifestsvc.LoadFile(abs)
			if err != nil {
				return err
			}
			if len(m.Automations) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no automations defined in manifest")
				return nil
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}

			sshDial := func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
				_ = ctx
				addr := net.JoinHostPort(host, strconv.Itoa(port))
				return store.DialSSH(addr, user)
			}

			ctx := scaffold.EnsurePlanCache(cmd.Context())

			targets := provisioning.BuildMachineTargets(m.Machines)
			provider, err := apply.ProviderFromManifest(m, abs)
			if err != nil {
				return err
			}
			if provider != nil {
				if err := provisioning.ResolveHostedInstances(ctx, provider, m.Prefix, targets); err != nil {
					return err
				}
			}

			resolvedEnv, err := manifestsvc.LoadEnvFile(abs, m)
			if err != nil {
				return fmt.Errorf("load env_file for automations: %w", err)
			}
			plan := planaut.BuildPlan(m.Automations, m.Machines, planaut.Options{
				SSHDial:        sshDial,
				ManifestDir:    filepath.Dir(abs),
				MachineTargets: targets,
				ResolvedEnv:    resolvedEnv,
			}, names)

			return planaut.Run(ctx, plan, planaut.RunOptions{
				DryRun:     dryRun,
				PrettyPlan: prettyPlan,
				Out:        cmd.OutOrStdout(),
				ProgressOut: os.Stderr,
				Confirm: func() (bool, error) {
					var confirm bool
					if err := huh.NewConfirm().
						Title("Run automations?").
						Affirmative("Yes").
						Negative("No").
						Value(&confirm).
						Run(); err != nil {
						return false, err
					}
					return confirm, nil
				},
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print plan and skip execution")
	cmd.Flags().BoolVar(&prettyPlan, "pretty-plan", false, "show plan in a scrollable Bubble Tea viewport")
	cmd.Flags().StringSliceVar(&names, "name", nil, "run only named automations (repeatable; selects manual ones too)")
	return cmd
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	return "", fmt.Errorf("specify manifest path: argument, or claws -f manifest.yml automations apply")
}
