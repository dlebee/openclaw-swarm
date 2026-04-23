package nodes

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	meshService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/mesh"
	"github.com/gluwa/openclaw-swarm2/internal/cli/cliutil"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
)

const nodeUnit = "openclaw-node"

func repairCmd(manifestFile *string) *cobra.Command {
	var nodeName string
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Force re-install and re-pair a node",
		Long: strings.TrimSpace(`
Reinstalls the openclaw-node systemd service on a manifest node and re-pairs it
with its gateway. Equivalent to running the bootstrap-node + pair-node steps of
'claws apply' with --force for a single node.

Use --name to select a node when the manifest declares more than one.`),
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

			nodeSpec, gw, nodeMach, gwMach, err := resolveNodeContext(m, nodeName)
			if err != nil {
				return err
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			ctxEP, resolver, err := cliutil.PrepareEndpoints(ctx, abs, m)
			if err != nil {
				return err
			}
			ctx = ctxEP

			nodeEP, err := resolver.Resolve(*nodeMach)
			if err != nil {
				return err
			}
			gwEP, err := resolver.Resolve(*gwMach)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			// --- read gateway token ---
			fmt.Fprintf(out, "Reading gateway token from %s ...\n", gw.Name)
			gwAddr := net.JoinHostPort(gwEP.Host, strconv.Itoa(gwEP.Port))
			gwClient, err := store.DialSSH(gwAddr, gwEP.User)
			if err != nil {
				return fmt.Errorf("SSH to gateway %s: %w", gwAddr, err)
			}
			gwHome, err := gwService.ResolveHome(gwClient)
			if err != nil {
				gwClient.Close()
				return fmt.Errorf("resolve gateway home: %w", err)
			}
			token, err := gwService.ReadToken(gwClient, gwHome)
			gwClient.Close()
			if err != nil {
				return fmt.Errorf("read gateway token: %w", err)
			}
			if token == "" {
				return fmt.Errorf("gateway token is empty (run 'claws apply' first)")
			}

			// --- resolve gateway internal host ---
			gwHost := resolveGatewayInternalHost(store, *gw, *gwMach, gwEP)
			if gwHost == "" {
				return fmt.Errorf("could not resolve internal gateway host for %q", gwMach.Name)
			}

			// --- bootstrap node ---
			fmt.Fprintf(out, "Reinstalling openclaw-node on %s (gateway host: %s) ...\n", nodeSpec.Name, gwHost)
			nodeAddr := net.JoinHostPort(nodeEP.Host, strconv.Itoa(nodeEP.Port))
			nodeClient, err := store.DialSSH(nodeAddr, nodeEP.User)
			if err != nil {
				return fmt.Errorf("SSH to node %s: %w", nodeAddr, err)
			}
			defer nodeClient.Close()

			var envPrefix string
			if gwService.NeedsInsecureWS(*gw) {
				envPrefix = "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 "
			}

			script := fmt.Sprintf(`set -euo pipefail
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export OPENCLAW_GATEWAY_TOKEN=%q
%s%sopenclaw node install --host %q --port 18789 --display-name %q --runtime node --force
`, token, common.OpenclawCLIPreamble(), envPrefix, gwHost, nodeSpec.Name)

			scriptOut, err := bash.RunOutput(nodeClient, script)
			if err != nil {
				return fmt.Errorf("openclaw node install failed: %w\n%s", err, scriptOut)
			}

			if err := gwService.EnsureNodeCompileCacheDir(nodeClient); err != nil {
				return fmt.Errorf("ensure compile cache dir: %w", err)
			}

			desiredEnv := repairNodeEnv(*gw)
			if len(desiredEnv) > 0 {
				if err := systemd.WriteEnvDropIn(nodeClient, nodeUnit, true, desiredEnv); err != nil {
					return fmt.Errorf("write env drop-in: %w", err)
				}
				if err := systemd.Restart(nodeClient, nodeUnit, true); err != nil {
					return fmt.Errorf("restart after env drop-in: %w", err)
				}
			}

			fmt.Fprintln(out, "Node service reinstalled.")

			// --- pair node ---
			fmt.Fprintf(out, "Pairing %s on gateway %s ...\n", nodeSpec.Name, gw.Name)
			gwClient2, err := store.DialSSH(gwAddr, gwEP.User)
			if err != nil {
				return fmt.Errorf("SSH to gateway for pairing: %w", err)
			}
			defer gwClient2.Close()

			if err := pollAndApprove(cmd, gwClient2, gwEP, nodeSpec.Name); err != nil {
				return err
			}

			// Restart the node daemon so it reconnects with the approved pairing.
			if err := systemd.Restart(nodeClient, nodeUnit, true); err != nil {
				return fmt.Errorf("restart node daemon after pairing: %w", err)
			}

			fmt.Fprintf(out, "Node %q repaired and paired.\n", nodeSpec.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&nodeName, "name", "", "node name as declared in the manifest")
	return cmd
}

func repairNodeEnv(gw manifestdata.Gateway) map[string]string {
	env := gwService.StartupOptimEnv()
	if gwService.NeedsInsecureWS(gw) {
		env["OPENCLAW_ALLOW_INSECURE_PRIVATE_WS"] = "1"
	}
	return env
}

func resolveGatewayInternalHost(
	store *state.Store,
	gw manifestdata.Gateway,
	gwMach manifestdata.Machine,
	gwEP cliutil.Endpoint,
) string {
	if h := strings.TrimSpace(gwMach.InternalHost); h != "" {
		return h
	}
	// On a headscale mesh, probe the gateway for its tailscale IP.
	if gw.Networking != nil && strings.EqualFold(strings.TrimSpace(gw.Networking.Mode), "headscale") {
		addr := net.JoinHostPort(gwEP.Host, strconv.Itoa(gwEP.Port))
		if client, err := store.DialSSH(addr, gwEP.User); err == nil {
			ip := meshService.TailscaleIP(client)
			client.Close()
			if ip != "" {
				return ip
			}
		}
	}
	if h := strings.TrimSpace(gwMach.Host); h != "" {
		return h
	}
	return gwEP.Host
}

func pollAndApprove(cmd *cobra.Command, gwClient *xssh.Client, gwEP cliutil.Endpoint, displayName string) error {
	reader := common.NewCLIConfigReader(nil)
	cfgHost := common.ConfigHost{Addr: gwEP.Host, Port: gwEP.Port, User: gwEP.User}
	out := cmd.OutOrStdout()

	const maxAttempts = 20
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		dl, err := reader.DeviceList(cmd.Context(), gwClient, cfgHost)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}

		for _, d := range dl.Paired {
			if d.DisplayName == displayName && d.ClientMode == "node" {
				fmt.Fprintf(out, "  %s already paired.\n", displayName)
				return nil
			}
		}

		for _, p := range dl.Pending {
			if p.DisplayName == displayName && p.Role == "node" {
				fmt.Fprintf(out, "  Approving %s ...\n", displayName)
				if err := gwService.ApproveDevice(gwClient, p.RequestID); err != nil {
					lastErr = err
					break
				}
				return nil
			}
		}

		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("pair: node %q not paired after %d attempts: %w", displayName, maxAttempts, lastErr)
	}
	return fmt.Errorf("pair: node %q did not appear as pending device after %d attempts", displayName, maxAttempts)
}

func resolveNodeContext(m *manifestdata.Manifest, name string) (
	*manifestdata.Node,
	*manifestdata.Gateway,
	*manifestdata.Machine,
	*manifestdata.Machine,
	error,
) {
	if len(m.Nodes) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("no nodes defined in manifest")
	}

	n, err := pickNode(m, name)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var nodeMach *manifestdata.Machine
	for i := range m.Machines {
		if m.Machines[i].Name == n.Reference {
			nodeMach = &m.Machines[i]
			break
		}
	}
	if nodeMach == nil {
		return nil, nil, nil, nil, fmt.Errorf("node %q: reference machine %q not found", n.Name, n.Reference)
	}

	var gw *manifestdata.Gateway
	for i := range m.Gateways {
		if m.Gateways[i].Name == n.Gateway {
			gw = &m.Gateways[i]
			break
		}
	}
	if gw == nil {
		return nil, nil, nil, nil, fmt.Errorf("node %q: gateway %q not found", n.Name, n.Gateway)
	}

	var gwMach *manifestdata.Machine
	for i := range m.Machines {
		if m.Machines[i].Name == gw.Reference {
			gwMach = &m.Machines[i]
			break
		}
	}
	if gwMach == nil {
		return nil, nil, nil, nil, fmt.Errorf("gateway %q: reference machine %q not found", gw.Name, gw.Reference)
	}

	return n, gw, nodeMach, gwMach, nil
}

func pickNode(m *manifestdata.Manifest, name string) (*manifestdata.Node, error) {
	if name != "" {
		for i := range m.Nodes {
			if m.Nodes[i].Name == name {
				return &m.Nodes[i], nil
			}
		}
		return nil, fmt.Errorf("no node named %q in manifest", name)
	}
	if len(m.Nodes) == 1 {
		return &m.Nodes[0], nil
	}
	opts := make([]huh.Option[int], len(m.Nodes))
	for i, n := range m.Nodes {
		opts[i] = huh.NewOption(fmt.Sprintf("%s -> %s (gw: %s)", n.Name, n.Reference, n.Gateway), i)
	}
	var idx int
	if err := huh.NewSelect[int]().
		Title("Which node?").
		Options(opts...).
		Value(&idx).
		Run(); err != nil {
		return nil, err
	}
	if idx < 0 || idx >= len(m.Nodes) {
		return nil, fmt.Errorf("invalid node selection")
	}
	return &m.Nodes[idx], nil
}
