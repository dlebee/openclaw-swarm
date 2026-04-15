package main

import (
	"os"

	manifestcmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/manifest"
	sshauth "github.com/gluwa/openclaw-swarm2/internal/cli/commands/ssh"
	"github.com/spf13/cobra"
)

func main() {
	var manifestFile string
	root := &cobra.Command{
		Use:   "claws",
		Short: "Claws — OpenClaw swarm CLI",
	}
	root.PersistentFlags().StringVarP(&manifestFile, "file", "f", "", "default manifest YAML for manifest commands")
	root.AddCommand(sshauth.AuthCmd())
	root.AddCommand(manifestcmd.ManifestCmd(&manifestFile))
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
