package nodes

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/huh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/cli/cliutil"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
)

// NodesCmd returns the `claws nodes` command group.
func NodesCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Node operations (list paired/pending devices on a gateway)",
	}
	cmd.AddCommand(listCmd(manifestFile))
	cmd.AddCommand(repairCmd(manifestFile))
	return cmd
}

func listCmd(manifestFile *string) *cobra.Command {
	var gwName string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List paired and pending nodes on a gateway",
		Long: strings.TrimSpace(`
Connects to the gateway via SSH and queries the device list. Shows all paired
nodes and any pending pairing requests. Use --gateway to select a specific
gateway when the manifest declares more than one.`),
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
			gw, mach, err := resolveGatewayAndMachine(m, gwName)
			if err != nil {
				return err
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}

			_, resolver, err := cliutil.PrepareEndpoints(cmd.Context(), abs, m)
			if err != nil {
				return err
			}
			ep, err := resolver.Resolve(*mach)
			if err != nil {
				return err
			}

			addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
			client, err := store.DialSSH(addr, ep.User)
			if err != nil {
				return fmt.Errorf("SSH to %s: %w", addr, err)
			}
			defer client.Close()

			reader := common.NewCLIConfigReader(nil)
			cfgHost := common.ConfigHost{Addr: ep.Host, Port: ep.Port, User: ep.User}
			dl, err := reader.DeviceList(cmd.Context(), client, cfgHost)
			if err != nil {
				return fmt.Errorf("query devices on %s: %w", gw.Name, err)
			}

			out := cmd.OutOrStdout()

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(dl)
			}

			pairedNodes := filterNodes(dl)
			if len(pairedNodes) == 0 && len(dl.Pending) == 0 {
				fmt.Fprintf(out, "No nodes on gateway %q.\n", gw.Name)
				return nil
			}

			if len(pairedNodes) > 0 {
				fmt.Fprintf(out, "Paired nodes on %q:\n", gw.Name)
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "  NAME\tMODE")
				for _, d := range pairedNodes {
					fmt.Fprintf(tw, "  %s\t%s\n", d.DisplayName, d.ClientMode)
				}
				tw.Flush()
			}

			if len(dl.Pending) > 0 {
				if len(pairedNodes) > 0 {
					fmt.Fprintln(out)
				}
				fmt.Fprintf(out, "Pending devices on %q:\n", gw.Name)
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "  NAME\tROLE\tREQUEST ID")
				for _, d := range dl.Pending {
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.DisplayName, d.Role, d.RequestID)
				}
				tw.Flush()
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&gwName, "gateway", "", "gateway name (auto-selected when only one exists)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output raw JSON")
	return cmd
}

func filterNodes(dl *common.DeviceList) []common.PairedDevice {
	if dl == nil {
		return nil
	}
	var out []common.PairedDevice
	for _, d := range dl.Paired {
		if d.ClientMode == "node" {
			out = append(out, d)
		}
	}
	return out
}

func resolveGatewayAndMachine(m *manifestdata.Manifest, name string) (*manifestdata.Gateway, *manifestdata.Machine, error) {
	gw, err := resolveGateway(m, name)
	if err != nil {
		return nil, nil, err
	}
	for i := range m.Machines {
		if m.Machines[i].Name == gw.Reference {
			return gw, &m.Machines[i], nil
		}
	}
	return nil, nil, fmt.Errorf("gateway %q: reference machine %q not found", gw.Name, gw.Reference)
}

func resolveGateway(m *manifestdata.Manifest, name string) (*manifestdata.Gateway, error) {
	if len(m.Gateways) == 0 {
		return nil, fmt.Errorf("no gateways defined in manifest")
	}
	if name != "" {
		for i := range m.Gateways {
			if m.Gateways[i].Name == name {
				return &m.Gateways[i], nil
			}
		}
		return nil, fmt.Errorf("no gateway named %q in manifest", name)
	}
	if len(m.Gateways) == 1 {
		return &m.Gateways[0], nil
	}
	opts := make([]huh.Option[int], 0, len(m.Gateways))
	for i, g := range m.Gateways {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s -> %s", g.Name, g.Reference), i))
	}
	var idx int
	if err := huh.NewSelect[int]().
		Title("Which gateway?").
		Options(opts...).
		Value(&idx).
		Run(); err != nil {
		return nil, err
	}
	if idx < 0 || idx >= len(m.Gateways) {
		return nil, fmt.Errorf("invalid gateway selection")
	}
	return &m.Gateways[idx], nil
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	return "", fmt.Errorf("specify manifest path: claws -f manifest.yml nodes list")
}
