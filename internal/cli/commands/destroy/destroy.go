package destroy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	clawdestroy "github.com/gluwa/openclaw-swarm2/internal/claws/plans/destroy"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// DestroyCmd returns the `claws destroy` command.
func DestroyCmd(manifestFile *string) *cobra.Command {
	var destroyAll, yes, dryRun bool
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy Linode instances tagged for this manifest prefix",
		Long: strings.TrimSpace(`
Lists instances tagged claws/<prefix> from the manifest, then either deletes all (--all)
or opens an interactive multi-select (space to toggle, enter to confirm).

Requires manifest linode_token_env and a reachable Linode API token.`),
		Args: cobra.MaximumNArgs(1),
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
			prov, err := linodeProviderFromManifest(m, abs)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			instances, err := clawdestroy.ListInstances(ctx, prov, m.Prefix)
			if err != nil {
				return err
			}
			if len(instances) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No Linode instances tagged %q.\n", clawdestroyListTagHint(m.Prefix))
				return nil
			}

			var toDestroy []hosting.Instance
			switch {
			case destroyAll:
				toDestroy = instances
			default:
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("destroy: stdin is not a TTY; use --all to destroy every instance for this prefix, or run from an interactive terminal")
				}
				toDestroy, err = PickInstancesMulti(instances)
				if err != nil {
					return err
				}
			}

			if len(toDestroy) == 0 {
				return nil
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Will destroy %d instance(s) for prefix %q:\n", len(toDestroy), m.Prefix)
			for _, inst := range toDestroy {
				fmt.Fprintln(out, "  "+clawdestroy.FormatInstanceLine(inst))
			}

			if dryRun {
				fmt.Fprintln(out, "Dry run: no instances deleted.")
				return nil
			}

			if !yes {
				fmt.Fprintf(out, "Proceed? [y/N]: ")
				var line string
				if _, scanErr := fmt.Scanln(&line); scanErr != nil {
					return scanErr
				}
				switch strings.ToLower(strings.TrimSpace(line)) {
				case "y", "yes":
				default:
					fmt.Fprintln(out, "Aborted.")
					return nil
				}
			}

			logW := io.Writer(cmd.ErrOrStderr())
			if err := clawdestroy.DeleteInstances(ctx, prov, toDestroy, logW); err != nil {
				return err
			}
			fmt.Fprintf(out, "Destroyed %d instance(s).\n", len(toDestroy))
			return nil
		},
	}
	cmd.Flags().BoolVar(&destroyAll, "all", false, "destroy every instance tagged for this manifest prefix (no picker)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation prompt after selection or --all")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print selected instances and exit without calling the API")
	return cmd
}

func clawdestroyListTagHint(prefix string) string {
	return fmt.Sprintf("claws/%s", strings.TrimSpace(prefix))
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	return "", fmt.Errorf("specify manifest path: argument, or claws -f manifest.yml destroy")
}

func linodeProviderFromManifest(m *manifestdata.Manifest, manifestAbsPath string) (hosting.Provider, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	if strings.TrimSpace(m.LinodeTokenEnv) == "" {
		return nil, fmt.Errorf("manifest linode_token_env is required for destroy")
	}
	tok, err := manifestsvc.LookupEnvFromManifest(manifestAbsPath, m, m.LinodeTokenEnv)
	if err != nil {
		return nil, err
	}
	return linode.NewProvider(tok), nil
}
