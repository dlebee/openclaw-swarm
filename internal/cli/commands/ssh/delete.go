package sshauth

import (
	"fmt"
	"os"
	"path/filepath"

	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/spf13/cobra"
)

func deleteCmd() *cobra.Command {
	var removeFiles bool
	cmd := &cobra.Command{
		Use:   "delete [name]",
		Short: "Remove an SSH identity from Claws state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			s, err := openStore()
			if err != nil {
				return err
			}
			ids := s.SSHIdentities()
			id, ok := ids[name]
			if !ok {
				return fmt.Errorf("unknown ssh identity %q", name)
			}
			wasCurrent := s.SSHCurrent() == name
			if err := s.RemoveSSHIdentity(name); err != nil {
				return err
			}
			if err := s.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed identity %q from state.\n", name)
			if wasCurrent {
				if cur := s.SSHCurrent(); cur != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Active SSH identity is now %q.\n", cur)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "No active SSH identity. Run: claws auth use <name> or claws auth generate <name>\n")
				}
			}
			if !removeFiles {
				return nil
			}
			priv, err := clawssh.ExpandPath(id.PrivateKeyPath)
			if err != nil {
				return err
			}
			dir := filepath.Dir(priv)
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("remove key directory: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted key files under %s\n", dir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&removeFiles, "remove-files", false, "also delete the key directory from disk (only for keys under the identity dir)")
	return cmd
}
