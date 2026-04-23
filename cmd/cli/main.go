package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	applycmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/apply"
	automationscmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/automations"
	channelscmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/channels"
	cleancmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/clean"
	destroycmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/destroy"
	gatewayscmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/gateways"
	githubcmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/github"
	manifestcmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/manifest"
	nodescmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/nodes"
	remotecmd "github.com/gluwa/openclaw-swarm2/internal/cli/commands/remote"
	sshauth "github.com/gluwa/openclaw-swarm2/internal/cli/commands/ssh"
	"github.com/gluwa/openclaw-swarm2/internal/perflog"
	"github.com/spf13/cobra"
)

func main() {
	var manifestFile, performanceLog string
	root := &cobra.Command{
		Use:   "claws",
		Short: "Claws — OpenClaw swarm CLI",
	}
	root.PersistentFlags().StringVarP(&manifestFile, "file", "f", "", "default manifest YAML for manifest commands")
	root.PersistentFlags().StringVar(&performanceLog, "performance-log", "", "append OpenClaw CLI timing lines to this file (default "+defaultPerformanceLog+" when the flag is set with no value)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("performance-log") {
			return nil
		}
		path := strings.TrimSpace(performanceLog)
		if path == "" {
			path = defaultPerformanceLog
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("performance log: %w", err)
		}
		perflog.SetWriter(f)
		return nil
	}
	root.AddCommand(sshauth.AuthCmd())
	root.AddCommand(manifestcmd.ManifestCmd(&manifestFile))
	root.AddCommand(applycmd.ApplyCmd(&manifestFile))
	root.AddCommand(automationscmd.AutomationsCmd(&manifestFile))
	root.AddCommand(destroycmd.DestroyCmd(&manifestFile))
	root.AddCommand(cleancmd.CleanCmd(&manifestFile))
	root.AddCommand(channelscmd.ChannelsCmd(&manifestFile))
	root.AddCommand(gatewayscmd.GatewaysCmd(&manifestFile))
	root.AddCommand(githubcmd.GitHubCmd(&manifestFile))
	root.AddCommand(nodescmd.NodesCmd(&manifestFile))
	root.AddCommand(remotecmd.SSHCmd(&manifestFile))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() { _ = perflog.Close() }()
	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

const defaultPerformanceLog = "openclaw-perf.log"
