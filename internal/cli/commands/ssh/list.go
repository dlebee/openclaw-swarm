package sshauth

import (
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered SSH identities",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			ids := s.SSHIdentities()
			cur := s.SSHCurrent()
			names := make([]string, 0, len(ids))
			for n := range ids {
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No SSH identities. Run: claws auth generate <name>")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CURRENT\tNAME\tPRIVATE KEY\tPUBLIC KEY")
			for _, name := range names {
				id := ids[name]
				mark := ""
				if name == cur {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, name, id.PrivateKeyPath, id.PublicKeyPath)
			}
			return w.Flush()
		},
	}
}
