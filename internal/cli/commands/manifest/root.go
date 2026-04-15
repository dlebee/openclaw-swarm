package manifestcmd

import "github.com/spf13/cobra"

// ManifestCmd builds `claws manifest` with show. manifestFile is the pointer to
// the root persistent -f/--file flag (may be nil for tests).
func ManifestCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Inspect manifest files",
	}
	cmd.AddCommand(showCmd(manifestFile))
	return cmd
}
