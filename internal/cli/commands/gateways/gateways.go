// Package gateways implements the `claws gateways` command group: dashboard
// and tui. Both target the reference machine of a gateway declared in the
// manifest, using the active SSH identity from the state store.
package gateways

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	gwsvc "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
)

// gatewayHTTPPort is the well-known local bind port of openclaw-gateway. It
// must stay in sync with gateway.gatewayPort in the apply plan.
const gatewayHTTPPort = 18789

// GatewaysCmd returns the `claws gateways` command group.
func GatewaysCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateways",
		Short: "Gateway operations (dashboard, tui)",
	}
	cmd.AddCommand(dashboardCmd(manifestFile))
	cmd.AddCommand(tuiCmd(manifestFile))
	return cmd
}

func dashboardCmd(manifestFile *string) *cobra.Command {
	var localPort int
	var name string
	cmd := &cobra.Command{
		Use:   "dashboard [gateway]",
		Short: "Open the gateway dashboard via SSH port forward",
		Long: strings.TrimSpace(`
Forwards the gateway's HTTP port (18789) to a local port via SSH and prints
the dashboard URL (including #token=... when the gateway auth token can be
read from openclaw.json). Keep the command running; press Ctrl+C to stop.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadManifest(manifestFile)
			if err != nil {
				return err
			}
			gw, mach, err := resolveGatewayAndMachine(m, argName(name, args))
			if err != nil {
				return err
			}

			host, port, user, err := sshEndpoint(mach)
			if err != nil {
				return err
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}
			id, err := store.ActiveSSHIdentity()
			if err != nil {
				return err
			}
			keyPath, err := clawssh.ExpandPath(id.PrivateKeyPath)
			if err != nil {
				return fmt.Errorf("expand private key path: %w", err)
			}

			token := readGatewayToken(store, host, port, user)

			dashURL := fmt.Sprintf("http://localhost:%d", localPort)
			if token != "" {
				dashURL = fmt.Sprintf("%s#token=%s", dashURL, token)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "⟳ forwarding %s (%s) port %d → localhost:%d ...\n",
				gw.Name, host, gatewayHTTPPort, localPort)
			fmt.Fprintf(out, "  Dashboard: %s\n", dashURL)
			if token == "" {
				fmt.Fprintln(out, "  (no gateway auth token yet — run 'claws apply' first)")
			}
			fmt.Fprintln(out, "  Press Ctrl+C to stop.")

			sshArgs := []string{
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-N",
				"-p", strconv.Itoa(port),
				"-L", fmt.Sprintf("%d:127.0.0.1:%d", localPort, gatewayHTTPPort),
				"-i", keyPath,
				fmt.Sprintf("%s@%s", user, host),
			}
			c := exec.Command("ssh", sshArgs...)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().IntVarP(&localPort, "port", "p", 18789, "local port to bind")
	cmd.Flags().StringVar(&name, "name", "", "gateway name (overrides positional arg)")
	return cmd
}

func tuiCmd(manifestFile *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "tui [gateway]",
		Short: "Open the OpenClaw TUI on a gateway via SSH",
		Long: strings.TrimSpace(`
Opens an interactive SSH session (with a TTY) against the gateway's reference
machine and runs 'openclaw tui'. Requires 'openclaw' to be installed on the
remote host (provided by 'claws apply').`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadManifest(manifestFile)
			if err != nil {
				return err
			}
			gw, mach, err := resolveGatewayAndMachine(m, argName(name, args))
			if err != nil {
				return err
			}

			host, port, user, err := sshEndpoint(mach)
			if err != nil {
				return err
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}
			id, err := store.ActiveSSHIdentity()
			if err != nil {
				return err
			}
			keyPath, err := clawssh.ExpandPath(id.PrivateKeyPath)
			if err != nil {
				return fmt.Errorf("expand private key path: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "⟳ opening TUI on %s (%s) ...\n", gw.Name, host)

			sshArgs := []string{
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-t",
				"-p", strconv.Itoa(port),
				"-i", keyPath,
				fmt.Sprintf("%s@%s", user, host),
				"openclaw tui",
			}
			c := exec.Command("ssh", sshArgs...)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "gateway name (overrides positional arg)")
	return cmd
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func loadManifest(manifestFile *string) (*manifestdata.Manifest, error) {
	if manifestFile == nil || strings.TrimSpace(*manifestFile) == "" {
		return nil, fmt.Errorf("specify manifest path: claws -f manifest.yml gateways ...")
	}
	abs, err := filepath.Abs(*manifestFile)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}
	return manifestsvc.LoadFile(abs)
}

// argName prefers --name when provided, else the first positional arg.
func argName(flagName string, args []string) string {
	if s := strings.TrimSpace(flagName); s != "" {
		return s
	}
	if len(args) >= 1 {
		return strings.TrimSpace(args[0])
	}
	return ""
}

func resolveGatewayAndMachine(m *manifestdata.Manifest, name string) (*manifestdata.Gateway, *manifestdata.Machine, error) {
	gw, err := resolveGateway(m, name)
	if err != nil {
		return nil, nil, err
	}
	mach := findMachine(m, gw.Reference)
	if mach == nil {
		return nil, nil, fmt.Errorf("gateway %q: reference machine %q not found", gw.Name, gw.Reference)
	}
	return gw, mach, nil
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
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s → %s", g.Name, g.Reference), i))
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

func findMachine(m *manifestdata.Manifest, name string) *manifestdata.Machine {
	for i := range m.Machines {
		if m.Machines[i].Name == name {
			return &m.Machines[i]
		}
	}
	return nil
}

func sshEndpoint(mach *manifestdata.Machine) (host string, port int, user string, err error) {
	host = strings.TrimSpace(mach.Host)
	if host == "" {
		return "", 0, "", fmt.Errorf("machine %q has no host", mach.Name)
	}
	port = mach.SSHPort
	if port == 0 {
		port = 22
	}
	user = strings.TrimSpace(mach.AgentUser)
	if user == "" {
		user = strings.TrimSpace(mach.SSHUser)
	}
	if user == "" {
		user = "root"
	}
	return host, port, user, nil
}

// readGatewayToken opens a short-lived SSH session to read the gateway auth
// token from ~/.openclaw/openclaw.json. Returns "" when unavailable — callers
// should continue without the token in that case (apply may not have run).
func readGatewayToken(store *state.Store, host string, port int, user string) string {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := store.DialSSH(addr, user)
	if err != nil {
		return ""
	}
	defer client.Close()
	home, err := gwsvc.ResolveHome(client)
	if err != nil {
		return ""
	}
	tok, _ := gwsvc.ReadToken(client, home)
	return tok
}
