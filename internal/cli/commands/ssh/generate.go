package sshauth

import (
	"fmt"

	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "generate [name]",
		Short: "Generate a new Ed25519 PEM key pair and register it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			keysRoot, err := clawssh.DefaultKeysRoot()
			if err != nil {
				return err
			}
			id, err := clawssh.GeneratePEMIdentity(keysRoot, name)
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			if err := s.PutSSHIdentity(name, id); err != nil {
				return err
			}
			if s.SSHCurrent() == "" {
				if err := s.UseSSHIdentity(name); err != nil {
					return err
				}
			}
			if err := s.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Identity %q created.\n", name)
			fmt.Fprintf(cmd.OutOrStdout(), "  Private: %s\n", id.PrivateKeyPath)
			fmt.Fprintf(cmd.OutOrStdout(), "  Public:  %s\n", id.PublicKeyPath)
			if s.SSHCurrent() == name {
				fmt.Fprintf(cmd.OutOrStdout(), "Active identity set to %q.\n", name)
			}
			return nil
		},
	}
}
