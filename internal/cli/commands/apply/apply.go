package apply

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
)

// ApplyCmd returns the `claws apply` command.
func ApplyCmd(manifestFile *string) *cobra.Command {
	var dryRun, prettyPlan, includeManualAutomations, assumeYes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply manifest infrastructure",
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

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}
			id, err := store.ActiveSSHIdentity()
			if err != nil {
				return fmt.Errorf("SSH identity: %w (try: claws ssh auth)", err)
			}
			pubPath, err := clawssh.ExpandPath(id.PublicKeyPath)
			if err != nil {
				return fmt.Errorf("public key path: %w", err)
			}
			pubBytes, err := os.ReadFile(pubPath)
			if err != nil {
				return fmt.Errorf("read SSH public key %q: %w", pubPath, err)
			}
			sshPub := strings.TrimSpace(string(pubBytes))

			prov, err := planapply.LinodeProviderFromManifest(m, abs)
			if err != nil {
				return err
			}

			sshDial := func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
				_ = ctx
				addr := net.JoinHostPort(host, strconv.Itoa(port))
				return store.DialSSH(addr, user)
			}

			plan, err := planapply.BuildPlan(planapply.BuildOptions{
				Manifest:                 m,
				ManifestPath:             abs,
				Provider:                 prov,
				SSHPubKey:                sshPub,
				SSHDial:                  sshDial,
				IncludeManualAutomations: includeManualAutomations,
			})
			if err != nil {
				return err
			}

			ctx := scaffold.EnsurePlanCache(cmd.Context())
			provisioning.SetSSHPool(ctx, clawssh.NewPool())

			return planapply.Run(ctx, plan, planapply.RunOptions{
				DryRun:      dryRun,
				PrettyPlan:  prettyPlan,
				Out:         cmd.OutOrStdout(),
				ProgressOut: os.Stderr,
				Confirm: func() (bool, error) {
					if assumeYes {
						return true, nil
					}
					var confirm bool
					if err := huh.NewConfirm().
						Title("Apply this plan?").
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
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print plan and skip cloud mutations")
	cmd.Flags().BoolVar(&prettyPlan, "pretty-plan", false, "show plan in a scrollable Bubble Tea viewport (TTY only; falls back to inline)")
	cmd.Flags().BoolVar(&includeManualAutomations, "include-manual-automations", false, "also run automations flagged manual: true")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip the Apply this plan? confirmation prompt")
	return cmd
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	return "", fmt.Errorf("specify manifest path: argument, or claws -f manifest.yml apply")
}
