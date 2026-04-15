package manifestcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func showCmd(manifestFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show [path]",
		Short: "Display a manifest in the terminal (Charm: scrollable, styled)",
		Long: `Load a YAML manifest and show it with Lip Gloss tables in a scrollable Bubble Tea viewport.

Examples:
  claws manifest show ./infra/manifest.yml
  claws -f manifest.yml manifest show    # uses root --file before the subcommand`,
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
			display := DisplayPath(abs)

			if !term.IsTerminal(int(os.Stdout.Fd())) {
				_, err := fmt.Fprint(cmd.OutOrStdout(), PlainText(display, m)+"\n")
				return err
			}
			return RunViewport(display, m)
		},
	}
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	return "", fmt.Errorf("specify manifest path: argument, or claws -f manifest.yml manifest show")
}
