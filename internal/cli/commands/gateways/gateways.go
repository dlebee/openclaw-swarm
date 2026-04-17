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
	"runtime"
	"strconv"
	"strings"
	"time"

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
	var openBrowser bool
	var printURL bool
	cmd := &cobra.Command{
		Use:   "dashboard [gateway]",
		Short: "Open the gateway dashboard via SSH port forward",
		Long: strings.TrimSpace(`
Forwards the gateway's HTTP port (18789) to a local port via SSH. By default
launches the system browser at http://localhost:<port>#token=... once the
tunnel is ready, and keeps the forward open until you press Ctrl+C. Pass
--no-open to skip the browser launch, or --print-url to also echo the URL
(with the auth token) to the console.`),
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
			if token == "" {
				fmt.Fprintln(out, "  (no gateway auth token yet — run 'claws apply' first)")
			}

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
			if err := c.Start(); err != nil {
				return fmt.Errorf("start ssh: %w", err)
			}

			// Wait for the local forward to accept connections before launching
			// the browser. On failure, still surface the URL so the user can
			// retry manually.
			ready := waitLocalPort(localPort, 10*time.Second)
			switch {
			case !ready:
				fmt.Fprintln(out, "  (tunnel did not become ready within 10s — check ssh output above)")
				if !printURL {
					fmt.Fprintf(out, "  Dashboard: %s\n", dashURL)
				}
			case openBrowser:
				if err := openInBrowser(dashURL); err != nil {
					fmt.Fprintf(out, "  could not open browser: %v\n", err)
					fmt.Fprintf(out, "  Dashboard: %s\n", dashURL)
				} else {
					fmt.Fprintln(out, "  Dashboard opened in browser.")
				}
			default:
				fmt.Fprintf(out, "  Dashboard: %s\n", dashURL)
			}
			if printURL && (ready && openBrowser) {
				fmt.Fprintf(out, "  Dashboard: %s\n", dashURL)
			}
			fmt.Fprintln(out, "  Press Ctrl+C to stop.")

			return c.Wait()
		},
	}
	cmd.Flags().IntVarP(&localPort, "port", "p", 18789, "local port to bind")
	cmd.Flags().StringVar(&name, "name", "", "gateway name (overrides positional arg)")
	cmd.Flags().BoolVar(&openBrowser, "open", true, "open the dashboard in the system browser")
	cmd.Flags().BoolVar(&printURL, "print-url", false, "print the dashboard URL (with token) even when the browser is opened")
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
		user = strings.TrimSpace(mach.BootstrapUser)
	}
	if user == "" {
		user = "root"
	}
	return host, port, user, nil
}

// waitLocalPort polls 127.0.0.1:port until a TCP connection succeeds, or the
// timeout elapses. Returns true on success.
func waitLocalPort(port int, timeout time.Duration) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// openInBrowser launches the platform default browser at the given URL.
// The returned error is non-nil when the launcher itself fails to start;
// success only guarantees that the helper command was invoked, not that the
// user's browser actually rendered the page.
func openInBrowser(url string) error {
	var bin string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		bin = "open"
		args = []string{url}
	case "windows":
		bin = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		bin = "xdg-open"
		args = []string{url}
	}
	c := exec.Command(bin, args...)
	return c.Start()
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
