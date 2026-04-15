package main

import (
	"os"

	sshauth "github.com/gluwa/openclaw-swarm2/internal/cli/commands/ssh"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "claws",
		Short: "Claws — OpenClaw swarm CLI",
	}
	root.AddCommand(sshauth.AuthCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
