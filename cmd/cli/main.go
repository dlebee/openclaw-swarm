package main

import (
	"os"

	applycmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/apply"
	automationscmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/automations"
	channelscmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/channels"
	cleancmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/clean"
	destroycmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/destroy"
	manifestcmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/manifest"
	remotecmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/remote"
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
	root.AddCommand(automationscmd.AutomationsCmd(&manifestFile))
	root.AddCommand(destroycmd.DestroyCmd(&manifestFile))
	root.AddCommand(cleancmd.CleanCmd(&manifestFile))
	root.AddCommand(channelscmd.ChannelsCmd(&manifestFile))
	root.AddCommand(remotecmd.SSHCmd(&manifestFile))
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
