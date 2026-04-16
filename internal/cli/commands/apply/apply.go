package apply

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
)

// ApplyCmd returns the `claws apply` command.
func ApplyCmd(manifestFile *string) *cobra.Command {
	var dryRun, prettyPlan bool
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
				Manifest:  m,
				Provider:  prov,
				SSHPubKey: sshPub,
				SSHDial:   sshDial,
			})
			if err != nil {
				return err
			}

			return planapply.Run(cmd.Context(), plan, planapply.RunOptions{
				DryRun:      dryRun,
				PrettyPlan:  prettyPlan,
				Out:         cmd.OutOrStdout(),
				ProgressOut: os.Stderr,
				Confirm: func() (bool, error) {
					fmt.Fprint(cmd.OutOrStdout(), "Proceed? [y/N]: ")
					var line string
					_, scanErr := fmt.Scanln(&line)
					if scanErr != nil {
						return false, scanErr
					}
					switch strings.ToLower(strings.TrimSpace(line)) {
					case "y", "yes":
						return true, nil
					default:
						return false, nil
					}
				},
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print plan and skip cloud mutations")
	cmd.Flags().BoolVar(&prettyPlan, "pretty-plan", false, "show plan in a scrollable Bubble Tea viewport (TTY only; falls back to inline)")
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
