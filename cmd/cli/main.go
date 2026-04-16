package main

import (
	"os"

	applycmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/apply"
	destroycmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/destroy"
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
	root.AddCommand(applycmd.ApplyCmd(&manifestFile))
	root.AddCommand(destroycmd.DestroyCmd(&manifestFile))
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
