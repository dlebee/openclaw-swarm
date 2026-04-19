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
	var (
		dryRun, prettyPlan, includeManualAutomations, assumeYes bool
		listPhases                                              bool
		onlyPhases, skipPhases                                  []string
	)
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

			prov, err := planapply.ProviderFromManifest(m, abs)
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

			names := plan.PhaseNames()
			if listPhases {
				for _, n := range names {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				return nil
			}
			onlyPhases = normalizePhaseList(onlyPhases)
			skipPhases = normalizePhaseList(skipPhases)
			if err := validatePhaseNames("phases", onlyPhases, names); err != nil {
				return err
			}
			if err := validatePhaseNames("skip-phases", skipPhases, names); err != nil {
				return err
			}

			ctx := scaffold.EnsurePlanCache(cmd.Context())
			provisioning.SetSSHPool(ctx, clawssh.NewPool())

			return planapply.Run(ctx, plan, planapply.RunOptions{
				DryRun:      dryRun,
				PrettyPlan:  prettyPlan,
				Out:         cmd.OutOrStdout(),
				ProgressOut: os.Stderr,
				OnlyPhases:  onlyPhases,
				SkipPhases:  skipPhases,
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
	cmd.Flags().StringSliceVar(&onlyPhases, "phases", nil, "comma-separated phases to run (everything else is skipped); use --list-phases to discover names")
	cmd.Flags().StringSliceVar(&skipPhases, "skip-phases", nil, "comma-separated phases to skip; runs every other phase")
	cmd.Flags().BoolVar(&listPhases, "list-phases", false, "print phase names for this manifest (in order) and exit")
	cmd.MarkFlagsMutuallyExclusive("phases", "skip-phases")
	return cmd
}

// normalizePhaseList trims whitespace and drops empty entries so splits like
// "--phases=,provisioning," don't surface ghost names in validation errors.
func normalizePhaseList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validatePhaseNames rejects unknown phase names with a helpful error that
// lists the real ones — the alternative (silent skip at scaffold layer) makes
// typos invisible until the run finishes doing nothing.
func validatePhaseNames(flag string, requested, available []string) error {
	if len(requested) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(available))
	for _, n := range available {
		known[n] = struct{}{}
	}
	var unknown []string
	for _, n := range requested {
		if _, ok := known[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("--%s: unknown phase(s) %s; available: %s",
		flag, strings.Join(unknown, ", "), strings.Join(available, ", "))
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
