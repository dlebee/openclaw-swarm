package remote

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/cli/cliutil"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/tmout"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
)

// TestSSHCmd returns `claws ssh test`, a quick connectivity probe that dials
// bootstrap_user and agent_user on a manifest machine and runs "whoami" to
// verify the active claws identity can reach both accounts. Useful for
// catching TMOUT kills (exit 142), bad authorized_keys, or wrong hostnames
// before running `claws apply`.
func TestSSHCmd(manifestFile *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Test SSH connectivity to a manifest machine",
		Long: strings.TrimSpace(`
Dials bootstrap_user and agent_user on a manifest machine using the active
claws SSH identity and runs "whoami" to verify both connections work.

Reports per-user pass/fail and elapsed time. Useful for diagnosing key
authorization issues, TMOUT kills (exit 142), or wrong host configuration
before running claws apply.`),
		Args: cobra.NoArgs,
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

			mach, err := resolveMachine(m, name)
			if err != nil {
				return err
			}

			store, err := state.OpenDefault()
			if err != nil {
				return err
			}
			if _, err := store.ActiveSSHIdentity(); err != nil {
				return fmt.Errorf("%w (try: claws auth generate <name>)", err)
			}

			_, resolver, err := cliutil.PrepareEndpoints(cmd.Context(), abs, m)
			if err != nil {
				return err
			}
			ep, err := resolver.Resolve(*mach)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
			fmt.Fprintf(out, "Machine:  %s\n", mach.Name)
			fmt.Fprintf(out, "Host:     %s\n", addr)
			fmt.Fprintln(out)

			anyFailed := false
			bootstrapUser := strings.TrimSpace(mach.BootstrapUser)
			if bootstrapUser == "" {
				bootstrapUser = "root"
			}

			for _, u := range testSSHUsers(*mach) {
				start := time.Now()
				whoami, testErr := sshWhoami(store, addr, u)
				elapsed := time.Since(start).Round(time.Millisecond)
				if testErr != nil {
					fmt.Fprintf(out, "  ✗  %-20s  (%s)  %v\n", u, elapsed, testErr)
					anyFailed = true
					continue
				}
				fmt.Fprintf(out, "  ✓  %-20s  (%s)  whoami=%s\n", u, elapsed, whoami)

				// After a successful bootstrap connection, check whether TMOUT
				// is set — it can kill long-running claws apply steps mid-flight.
				if u == bootstrapUser {
					client, dialErr := store.DialSSH(addr, u)
					if dialErr == nil {
						tmoutSet, tmoutErr := tmout.IsSet(client)
						client.Close()
						switch {
						case tmoutErr != nil:
							fmt.Fprintf(out, "  ?  %-20s  TMOUT check failed: %v\n", "tmout", tmoutErr)
						case tmoutSet:
							fmt.Fprintf(out, "  ⚠  %-20s  TMOUT is set — add 'options: unset_tmout: true' to your manifest\n", "tmout")
						default:
							fmt.Fprintf(out, "  ✓  %-20s  TMOUT not set\n", "tmout")
						}
					}
				}
			}
			fmt.Fprintln(out)
			if anyFailed {
				return fmt.Errorf("one or more SSH tests failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "machine name as declared in the manifest (default: auto-select)")
	return cmd
}

// testSSHUsers returns [bootstrap_user, agent_user] in that order, deduped.
// Bootstrap first because it's the provisioning identity — if it fails,
// apply will fail too, so it's the most important one to verify.
func testSSHUsers(m manifestdata.Machine) []string {
	bootstrap := strings.TrimSpace(m.BootstrapUser)
	if bootstrap == "" {
		bootstrap = "root"
	}
	agent := strings.TrimSpace(m.AgentUser)

	seen := make(map[string]bool, 2)
	var out []string
	for _, u := range []string{bootstrap, agent} {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// sshWhoami dials addr as user using the active claws identity, runs "whoami",
// and returns the trimmed output.
func sshWhoami(store *state.Store, addr, user string) (string, error) {
	client, err := store.DialSSH(addr, user)
	if err != nil {
		return "", err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	if err := sess.Run("whoami"); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
