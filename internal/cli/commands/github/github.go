// Package github implements the `claws github` command group. Currently
// exposes a single `setup` subcommand that installs the GitHub CLI and runs
// an interactive device-flow login on a selection of manifest machines,
// then distributes the resulting token to the rest — ported from
// openclaw-swarm/cli/cmd/claws/gh.go, adapted to openclaw-swarm2's
// state-store-backed SSH identity and manifest/machine types.
package github

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
)

// GitHubCmd returns the `claws github` command group.
func GitHubCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "GitHub CLI helpers for manifest machines",
	}
	cmd.AddCommand(setupCmd(manifestFile))
	return cmd
}

func setupCmd(manifestFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install GitHub CLI and login interactively on manifest machines",
		Long: strings.TrimSpace(`
Picks machines from the manifest, installs the GitHub CLI (gh) if missing,
then runs an interactive device-flow login on the first selected machine.
After the first machine is authenticated the token is automatically
distributed to the remaining selected machines via 'gh auth login --with-token'.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			m, err := loadManifest(manifestFile)
			if err != nil {
				return err
			}
			if len(m.Machines) == 0 {
				fmt.Fprintln(out, "No machines defined in the manifest.")
				return nil
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

			// Probe SSH reachability with a quick TCP dial. Anything that
			// doesn't respond within the probe window is silently skipped —
			// this mirrors the old openclaw-swarm 'status.ProbeMachine' gate
			// but without pulling in the full status package.
			type target struct {
				machine *manifestdata.Machine
				host    string
				port    int
				user    string
			}
			var reachable []target
			for i := range m.Machines {
				mach := &m.Machines[i]
				host, port, user, err := sshEndpoint(mach)
				if err != nil {
					continue
				}
				if !dialOK(host, port, 3*time.Second) {
					continue
				}
				reachable = append(reachable, target{machine: mach, host: host, port: port, user: user})
			}
			if len(reachable) == 0 {
				fmt.Fprintln(out, "No reachable machines.")
				return nil
			}

			opts := make([]huh.Option[int], 0, len(reachable))
			for i, t := range reachable {
				opts = append(opts, huh.NewOption(
					fmt.Sprintf("%s @ %s", t.machine.Name, t.host), i,
				).Selected(true))
			}
			var idxs []int
			if err := huh.NewMultiSelect[int]().
				Title("Install gh + login on which machines?").
				Options(opts...).
				Value(&idxs).
				Run(); err != nil {
				return err
			}
			if len(idxs) == 0 {
				fmt.Fprintln(out, "No selection — cancelled.")
				return nil
			}
			selected := make([]target, 0, len(idxs))
			for _, i := range idxs {
				selected = append(selected, reachable[i])
			}

			// ── install gh on every selected machine ──────────────────────
			fmt.Fprintln(out, "\n── Installing gh CLI ──")
			for _, t := range selected {
				client, err := dialTarget(store, t.host, t.port, t.user)
				if err != nil {
					fmt.Fprintf(out, "  ✗ %s: connect: %s\n", t.machine.Name, err)
					continue
				}
				probe, err := bash.RunOutput(client,
					`command -v gh >/dev/null 2>&1 && echo installed || echo missing`)
				if err == nil && strings.TrimSpace(probe) == "installed" {
					fmt.Fprintf(out, "  = %s: gh already installed\n", t.machine.Name)
					_ = client.Close()
					continue
				}

				fmt.Fprintf(out, "  ⟳ %s: installing gh ...\n", t.machine.Name)
				installOut, err := bash.RunOutput(client, installGHScript)
				_ = client.Close()
				if err != nil {
					fmt.Fprintf(out, "  ✗ %s: install failed: %s\n%s\n",
						t.machine.Name, err, strings.TrimSpace(installOut))
					continue
				}
				fmt.Fprintf(out, "  ✓ %s: %s\n", t.machine.Name, lastNonEmpty(installOut))
			}

			// ── interactive login on first machine ────────────────────────
			fmt.Fprintln(out, "\n── GitHub authentication ──")
			first := selected[0]
			fmt.Fprintf(out, "  Logging in interactively on %s (%s@%s) ...\n",
				first.machine.Name, first.user, first.host)
			fmt.Fprintln(out, "  (complete the device flow in your browser)")

			if err := execSSHWithCommand(first.host, first.port, first.user, keyPath, "gh auth login"); err != nil {
				return fmt.Errorf("interactive login on %s failed: %w", first.machine.Name, err)
			}

			// Grab the token from the authenticated machine.
			client, err := dialTarget(store, first.host, first.port, first.user)
			if err != nil {
				return fmt.Errorf("reconnect to %s: %w", first.machine.Name, err)
			}
			tokenOut, tokErr := bash.RunOutput(client, `gh auth token 2>/dev/null`)
			token := strings.TrimSpace(tokenOut)
			if tokErr != nil || token == "" {
				_ = client.Close()
				fmt.Fprintln(out,
					"\n  ⚠ Could not read token from first machine — login remaining machines manually.")
				return nil
			}
			verifyOut, _ := bash.RunOutput(client, `gh auth status 2>&1`)
			_ = client.Close()
			fmt.Fprintf(out, "\n  ✓ %s: %s\n", first.machine.Name, firstNonEmpty(verifyOut))

			// ── distribute token to the remaining machines ────────────────
			if len(selected) > 1 {
				fmt.Fprintln(out, "\n── Distributing token ──")
				for _, t := range selected[1:] {
					c, err := dialTarget(store, t.host, t.port, t.user)
					if err != nil {
						fmt.Fprintf(out, "  ✗ %s: connect: %s\n", t.machine.Name, err)
						continue
					}
					// `gh auth login --with-token` reads the token from stdin;
					// we feed it via a here-string so the secret never hits argv.
					distScript := fmt.Sprintf(
						`gh auth login --with-token <<'OC_GH_TOKEN_EOF' 2>&1
%s
OC_GH_TOKEN_EOF`, token)
					distOut, err := bash.RunOutput(c, distScript)
					if err != nil {
						fmt.Fprintf(out, "  ✗ %s: %s\n%s\n",
							t.machine.Name, err, strings.TrimSpace(distOut))
						_ = c.Close()
						continue
					}
					vOut, _ := bash.RunOutput(c, `gh auth status 2>&1`)
					_ = c.Close()
					fmt.Fprintf(out, "  ✓ %s: %s\n", t.machine.Name, firstNonEmpty(vOut))
				}
			}

			fmt.Fprintln(out, "\nDone.")
			return nil
		},
	}
	return cmd
}

// ---------------------------------------------------------------------------
// install script
// ---------------------------------------------------------------------------

// installGHScript pulls gh from the official apt repository, which is what
// the GitHub CLI docs recommend for Debian/Ubuntu hosts. Copied verbatim from
// openclaw-swarm/cli/cmd/claws/gh.go so behaviour stays identical.
const installGHScript = `set -e
export DEBIAN_FRONTEND=noninteractive
(type -p wget >/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq wget))
sudo mkdir -p -m 755 /etc/apt/keyrings
out=$(mktemp)
wget -qO "$out" https://cli.github.com/packages/githubcli-archive-keyring.gpg
sudo install -o root -g root -m 644 "$out" /etc/apt/keyrings/githubcli-archive-keyring.gpg
rm -f "$out"
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list >/dev/null
sudo apt-get update -qq
sudo apt-get install -y -qq gh
gh --version
`

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func loadManifest(manifestFile *string) (*manifestdata.Manifest, error) {
	if manifestFile == nil || strings.TrimSpace(*manifestFile) == "" {
		return nil, fmt.Errorf("specify manifest path: claws -f manifest.yml github ...")
	}
	abs, err := filepath.Abs(*manifestFile)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}
	return manifestsvc.LoadFile(abs)
}

// sshEndpoint resolves a machine's SSH host/port/user tuple, applying the
// same fallbacks used elsewhere in the codebase (agent_user → ssh_user → root,
// default port 22). Returns an error when the machine has no host.
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

// dialOK performs a short TCP connect to host:port. Used as a cheap liveness
// gate before showing the multi-select picker.
func dialOK(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// dialTarget opens an SSH client via the state store's active identity.
func dialTarget(store *state.Store, host string, port int, user string) (*xssh.Client, error) {
	return store.DialSSH(net.JoinHostPort(host, strconv.Itoa(port)), user)
}

// execSSHWithCommand runs an interactive SSH session that executes a single
// command with full TTY forwarding (stdin/stdout/stderr wired to the local
// terminal). Required for `gh auth login` which presents a device-flow
// prompt the user has to interact with locally.
func execSSHWithCommand(host string, port int, user, keyPath, command string) error {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-t",
		"-p", strconv.Itoa(port),
		"-i", keyPath,
		fmt.Sprintf("%s@%s", user, host),
		command,
	}
	c := exec.Command("ssh", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func firstNonEmpty(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return strings.TrimSpace(s)
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return strings.TrimSpace(s)
}
