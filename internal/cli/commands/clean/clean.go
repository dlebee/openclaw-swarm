package clean

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/channels"
	"github.com/gluwa/openclaw-swarm2/internal/cli/cliutil"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
)

// CleanCmd returns the `claws clean` command.
func CleanCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean [channels]",
		Short: "Remove resources that are live but no longer in the manifest",
		Long: strings.TrimSpace(`
Detects orphans (present on the gateway, absent from the manifest)
and lets you choose what to delete.

  channels   Channel accounts on gateways not listed under gateways[].channels`),
		Args: cobra.ExactArgs(1),
	}
	var yes bool
	cmd.PersistentFlags().BoolVar(&yes, "yes", false, "skip prompts and remove all detected orphans")

	cmd.AddCommand(cleanChannelsCmd(manifestFile, &yes))
	return cmd
}

func cleanChannelsCmd(manifestFile *string, yes *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "channels",
		Short: "Remove channel accounts present on gateways but not in the manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveManifestPath(manifestFile, args)
			if err != nil {
				return err
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("manifest path: %w", err)
			}
			m, err := manifestsvc.LoadFile(abs)
			if err != nil {
				return err
			}
			store, err := state.OpenDefault()
			if err != nil {
				return err
			}
			dial := func(_ context.Context, host string, port int, user string) (*xssh.Client, error) {
				addr := net.JoinHostPort(host, strconv.Itoa(port))
				return store.DialSSH(addr, user)
			}
			ctx, resolver, err := cliutil.PrepareEndpoints(cmd.Context(), abs, m)
			if err != nil {
				return err
			}
			return runCleanChannels(ctx, m, resolver, dial, cmd, *yes)
		},
	}
}

type orphanChannel struct {
	GatewayName string
	Kind        string
	Name        string
}

func runCleanChannels(
	ctx context.Context,
	m *manifestdata.Manifest,
	resolver *cliutil.EndpointResolver,
	dial func(ctx context.Context, host string, port int, user string) (*xssh.Client, error),
	cmd *cobra.Command,
	yes bool,
) error {
	want := manifestChannelKeys(m)

	machByName := make(map[string]manifestdata.Machine, len(m.Machines))
	for _, mach := range m.Machines {
		machByName[mach.Name] = mach
	}

	var orphans []orphanChannel
	type gwClient struct {
		client *xssh.Client
		gw     manifestdata.Gateway
	}
	var clients []gwClient

	for _, gw := range m.Gateways {
		mach, ok := machByName[gw.Reference]
		if !ok {
			continue
		}
		ep, err := resolver.Resolve(mach)
		if err != nil {
			// A gateway whose reference VM isn't running yet isn't an
			// orphan — it's just not reachable. Warn and move on so the
			// rest of the manifest still gets cleaned.
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipping gateway %q: %v\n", gw.Name, err)
			continue
		}

		client, err := dial(ctx, ep.Host, ep.Port, ep.User)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: cannot reach gateway %q (%s): %v\n", gw.Name, ep.Host, err)
			continue
		}
		clients = append(clients, gwClient{client: client, gw: gw})

		accounts, err := channels.ListChannelAccounts(client)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: listing channels on gateway %q: %v\n", gw.Name, err)
			continue
		}
		for kind, names := range accounts {
			for _, name := range names {
				key := strings.ToLower(kind) + "/" + name
				if _, ok := want[key]; ok {
					continue
				}
				orphans = append(orphans, orphanChannel{
					GatewayName: gw.Name,
					Kind:        kind,
					Name:        name,
				})
			}
		}
	}
	defer func() {
		for _, gc := range clients {
			gc.client.Close()
		}
	}()

	if len(orphans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Nothing to clean — no orphaned channel accounts found.")
		return nil
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Orphans (live but not in manifest):")
	for i, o := range orphans {
		fmt.Fprintf(cmd.OutOrStdout(), "  [%d] gateway=%s  %s/%s\n", i, o.GatewayName, o.Kind, o.Name)
	}

	selected := allIndices(orphans)
	if !yes {
		opts := make([]huh.Option[int], len(orphans))
		for i, o := range orphans {
			opts[i] = huh.NewOption(fmt.Sprintf("gateway=%s  %s/%s", o.GatewayName, o.Kind, o.Name), i)
		}
		var picked []int
		if err := huh.NewMultiSelect[int]().
			Title("Select channel accounts to remove").
			Options(opts...).
			Value(&picked).
			Run(); err != nil {
			return err
		}
		if len(picked) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No selection — cancelled.")
			return nil
		}
		var confirm bool
		if err := huh.NewConfirm().
			Title("Remove selected channel accounts?").
			Affirmative("Remove").
			Negative("Cancel").
			Value(&confirm).
			Run(); err != nil {
			return err
		}
		if !confirm {
			fmt.Fprintln(cmd.OutOrStdout(), "Cancelled.")
			return nil
		}
		selected = picked
	}

	gwClients := make(map[string]*xssh.Client, len(clients))
	for _, gc := range clients {
		gwClients[gc.gw.Name] = gc.client
	}

	for _, idx := range selected {
		o := orphans[idx]
		client, ok := gwClients[o.GatewayName]
		if !ok {
			return fmt.Errorf("no SSH client for gateway %s", o.GatewayName)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Removing %s/%s on gateway %s ...\n", o.Kind, o.Name, o.GatewayName)
		if err := channels.RemoveChannelAccount(client, o.Kind, o.Name); err != nil {
			return fmt.Errorf("gateway %s: %w", o.GatewayName, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Removed %s/%s\n", o.Kind, o.Name)
	}
	return nil
}

func manifestChannelKeys(m *manifestdata.Manifest) map[string]struct{} {
	out := make(map[string]struct{})
	for _, gw := range m.Gateways {
		for _, ch := range gw.Channels {
			key := strings.ToLower(string(ch.Kind)) + "/" + ch.Name
			out[key] = struct{}{}
		}
	}
	return out
}

func allIndices[T any](s []T) []int {
	out := make([]int, len(s))
	for i := range s {
		out[i] = i
	}
	return out
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	return "", fmt.Errorf("specify manifest path: argument, or claws -f manifest.yml clean")
}
