package channels

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	chsvc "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/channels"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
)

// ChannelsCmd returns the `claws channels` command group.
func ChannelsCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channels",
		Short: "Channel management (channel creation is handled by claws apply)",
	}
	cmd.AddCommand(pairCmd(manifestFile))
	return cmd
}

func pairCmd(manifestFile *string) *cobra.Command {
	var chName, code string
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Complete interactive channel pairing on the gateway host",
		Long: strings.TrimSpace(`
Runs 'openclaw pairing approve' on the gateway that owns the selected channel.
After 'claws apply' registers channel accounts, some integrations (e.g. Telegram)
require a one-time pairing code to activate the bot.`),
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
			gw, ch, err := resolveChannelSelection(m, chName)
			if err != nil {
				return err
			}
			if code == "" {
				if err := huh.NewInput().
					Title("Pairing code").
					Value(&code).
					Run(); err != nil {
					return fmt.Errorf("code prompt: %w", err)
				}
			}
			code = strings.TrimSpace(code)
			if code == "" {
				return fmt.Errorf("pairing code is empty")
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}

			machByName := make(map[string]manifestdata.Machine, len(m.Machines))
			for _, mach := range m.Machines {
				machByName[mach.Name] = mach
			}
			mach, ok := machByName[gw.Reference]
			if !ok {
				return fmt.Errorf("unknown reference machine %q for gateway %q", gw.Reference, gw.Name)
			}

			host := strings.TrimSpace(mach.Host)
			if host == "" {
				return fmt.Errorf("machine %q has no host", mach.Name)
			}
			port := mach.SSHPort
			if port == 0 {
				port = 22
			}
			user := strings.TrimSpace(mach.SSHUser)
			if user == "" {
				user = "root"
			}

			addr := net.JoinHostPort(host, strconv.Itoa(port))
			client, err := store.DialSSH(addr, user)
			if err != nil {
				return fmt.Errorf("SSH to %s: %w", addr, err)
			}
			defer client.Close()

			kind := string(ch.Kind)
			fmt.Fprintf(cmd.OutOrStdout(), "Pairing channel %q (%s) on gateway %q ...\n", ch.Name, kind, gw.Name)
			if err := chsvc.PairChannel(client, kind, code); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Pairing command completed.")
			return nil
		},
	}
	cmd.Flags().StringVar(&chName, "name", "", "channel name as declared in the manifest (channels[].name)")
	cmd.Flags().StringVar(&code, "code", "", "pairing code (if omitted, you will be prompted)")
	return cmd
}

type channelMatch struct {
	gw *manifestdata.Gateway
	ch *manifestdata.Channel
}

func resolveChannelSelection(m *manifestdata.Manifest, name string) (*manifestdata.Gateway, *manifestdata.Channel, error) {
	var matches []channelMatch
	for i := range m.Gateways {
		gw := &m.Gateways[i]
		for j := range gw.Channels {
			ch := &gw.Channels[j]
			matches = append(matches, channelMatch{gw: gw, ch: ch})
		}
	}
	if len(matches) == 0 {
		return nil, nil, fmt.Errorf("no channels defined in manifest")
	}
	if name != "" {
		for _, cm := range matches {
			if cm.ch.Name == name {
				return cm.gw, cm.ch, nil
			}
		}
		return nil, nil, fmt.Errorf("no channel named %q in manifest", name)
	}
	if len(matches) == 1 {
		return matches[0].gw, matches[0].ch, nil
	}
	opts := make([]huh.Option[int], len(matches))
	for i, cm := range matches {
		label := fmt.Sprintf("%s / %s (%s)", cm.gw.Name, cm.ch.Name, cm.ch.Kind)
		opts[i] = huh.NewOption(label, i)
	}
	var idx int
	if err := huh.NewSelect[int]().
		Title("Which channel?").
		Options(opts...).
		Value(&idx).
		Run(); err != nil {
		return nil, nil, err
	}
	if idx < 0 || idx >= len(matches) {
		return nil, nil, fmt.Errorf("invalid channel selection")
	}
	return matches[idx].gw, matches[idx].ch, nil
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	return "", fmt.Errorf("specify manifest path: argument, or claws -f manifest.yml channels pair")
}
