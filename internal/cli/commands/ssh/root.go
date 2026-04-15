package sshauth

import (
	"github.com/spf13/cobra"
)

// AuthCmd builds `claws auth` with generate, use, list, delete.
func AuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage Claws authentication (SSH identities)",
	}
	cmd.AddCommand(
		generateCmd(),
		useCmd(),
		listCmd(),
		deleteCmd(),
	)
	return cmd
}
