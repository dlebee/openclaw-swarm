package remote

import (
	"context"
	"errors"
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
	xssh "golang.org/x/crypto/ssh"
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

			ctx := cmd.Context()
			anyFailed := false
			bootstrapUser := strings.TrimSpace(mach.BootstrapUser)
			if bootstrapUser == "" {
				bootstrapUser = "root"
			}

			for _, u := range testSSHUsers(*mach) {
				if ctx.Err() != nil {
					fmt.Fprintln(out, "Cancelled.")
					return ctx.Err()
				}

				start := time.Now()
				whoami, testErr := sshWhoamiCtx(ctx, store, addr, u)
				elapsed := time.Since(start).Round(time.Millisecond)
				if errors.Is(testErr, context.Canceled) || errors.Is(testErr, context.DeadlineExceeded) {
					fmt.Fprintf(out, "  •  %-20s  (%s)  cancelled\n", u, elapsed)
					return testErr
				}
				if testErr != nil {
					fmt.Fprintf(out, "  ✗  %-20s  (%s)  %v\n", u, elapsed, testErr)
					anyFailed = true
					continue
				}
				fmt.Fprintf(out, "  ✓  %-20s  (%s)  whoami=%s\n", u, elapsed, whoami)

				// After a successful bootstrap connection, check whether TMOUT
				// is set — it can kill long-running claws apply steps mid-flight.
				if u == bootstrapUser {
					if ctx.Err() != nil {
						fmt.Fprintln(out, "Cancelled.")
						return ctx.Err()
					}
					tmoutSet, tmoutErr := tmoutCheckCtx(ctx, store, addr, u)
					switch {
					case errors.Is(tmoutErr, context.Canceled), errors.Is(tmoutErr, context.DeadlineExceeded):
						fmt.Fprintln(out, "Cancelled.")
						return tmoutErr
					case tmoutErr != nil:
						fmt.Fprintf(out, "  ?  %-20s  TMOUT check failed: %v\n", "tmout", tmoutErr)
					case tmoutSet:
						fmt.Fprintf(out, "  ⚠  %-20s  TMOUT is set — add 'options: unset_tmout: true' to your manifest\n", "tmout")
					default:
						fmt.Fprintf(out, "  ✓  %-20s  TMOUT not set\n", "tmout")
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

// sshWhoamiCtx dials addr as user using the active claws identity, runs
// "whoami", and returns the trimmed output. It honours ctx cancellation:
// if ctx fires while we're blocked on the dial or on Run, a watcher
// goroutine closes the underlying client which unblocks the call with an
// error and we return ctx.Err() instead.
func sshWhoamiCtx(ctx context.Context, store *state.Store, addr, user string) (string, error) {
	return runWithSSHCtx(ctx, store, addr, user, func(client *xssh.Client) (string, error) {
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
	})
}

// tmoutCheckCtx is a context-aware variant of tmout.IsSet that returns
// (set, err). Cancellation closes the client to unblock the SSH session.
func tmoutCheckCtx(ctx context.Context, store *state.Store, addr, user string) (bool, error) {
	out, err := runWithSSHCtx(ctx, store, addr, user, func(client *xssh.Client) (string, error) {
		set, err := tmout.IsSet(client)
		if err != nil {
			return "", err
		}
		if set {
			return "1", nil
		}
		return "0", nil
	})
	if err != nil {
		return false, err
	}
	return out == "1", nil
}

// runWithSSHCtx opens an SSH client to addr as user and invokes fn with it.
// A watcher goroutine waits on ctx.Done; if ctx is cancelled while fn is
// blocked (typical for slow dials or hung Run calls) it closes the client,
// which unblocks the operation with an error. The returned error is
// ctx.Err() when cancellation won the race, fn's error otherwise.
//
// Note: store.DialSSH itself doesn't accept a context, so cancellation
// before the dial returns can't actually abort the TCP/SSH handshake — but
// once the client exists the watcher reliably interrupts subsequent Run
// calls. For dial-stage cancellation the goroutine will exit naturally
// after the dial times out (typically 10–30s); from the user's POV the
// CLI returns immediately because we select on ctx in the caller.
func runWithSSHCtx[T any](ctx context.Context, store *state.Store, addr, user string, fn func(*xssh.Client) (T, error)) (T, error) {
	var zero T
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)

	go func() {
		client, err := store.DialSSH(addr, user)
		if err != nil {
			ch <- result{zero, err}
			return
		}
		defer client.Close()

		// Watcher: if ctx fires while fn is blocked, close the client to
		// unblock it. The done channel guarantees the goroutine exits when
		// fn completes normally (avoiding a leak).
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = client.Close()
			case <-done:
			}
		}()

		v, err := fn(client)
		ch <- result{v, err}
	}()

	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case r := <-ch:
		return r.v, r.err
	}
}
