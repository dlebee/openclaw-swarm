package sshauth

import (
	"fmt"

	"github.com/spf13/cobra"
)

func useCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use [name]",
		Short: "Set the active SSH identity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			s, err := openStore()
			if err != nil {
				return err
			}
			if err := s.UseSSHIdentity(name); err != nil {
				return err
			}
			if err := s.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Active SSH identity: %s\n", name)
			return nil
		},
	}
}
