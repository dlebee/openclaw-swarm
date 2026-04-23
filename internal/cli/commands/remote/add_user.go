package remote

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/gluwa/openclaw-swarm2/internal/cli/cliutil"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshkeys"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
	xssh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// AddUserCmd returns `claws ssh add-user`, which appends a supplied SSH
// public key to authorized_keys on every manifest machine for BOTH the
// agent user and the bootstrap user (deduped). It dials each user with
// the currently-active identity — that identity must already be authorized
// on both users (which is what provisioning + ensure-agent-user guarantees
// during `claws apply`), so this is strictly a "grant another operator
// access" primitive, not a recovery tool.
//
// Why both users: provisioning adds the initial operator key to
// /root/.ssh/authorized_keys, and ensure-agent-user copies it into
// /home/<agent>/.ssh/authorized_keys. Security hardening does not disable
// key-based root login. Mirroring that pair for every new operator keeps
// apply's invariant intact: anyone who holds a claws-authorized key can
// reach the machine as either user, the same way the first operator can.
func AddUserCmd(manifestFile *string) *cobra.Command {
	var (
		pubKeyFile string
		pubKeyLine string
		machine    string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "add-user",
		Short: "Authorize an additional SSH public key on every manifest machine",
		Long: strings.TrimSpace(`
Appends a public key to authorized_keys on every machine in the manifest,
for both the agent_user and the bootstrap_user (deduped when they are the
same account). Dials as the currently-active SSH identity. Idempotent:
a user where the key is already present is reported and left alone.

Provide the key via --pubkey <file>, --pubkey-line "ssh-ed25519 AAAA...",
or leave both unset to be prompted interactively.
`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if pubKeyFile != "" && pubKeyLine != "" {
				return errors.New("--pubkey and --pubkey-line are mutually exclusive")
			}

			path, err := resolveManifestPath(manifestFile, nil)
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

			line, err := resolvePubKey(pubKeyFile, pubKeyLine, stdinIsTTY())
			if err != nil {
				return err
			}
			parsed, comment, err := parseAuthorizedKeyLine(line)
			if err != nil {
				return err
			}

			targets, err := selectMachines(m, machine)
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

			fingerprint := xssh.FingerprintSHA256(parsed)
			label := comment
			if label == "" {
				label = "(no comment)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pubkey %s %s\n", fingerprint, label)
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "Dry run — no machines will be modified.")
			}

			var failed []string
			for _, mach := range targets {
				ep, err := resolver.Resolve(mach)
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s  SKIP  %v\n", mach.Name, err)
					failed = append(failed, mach.Name)
					continue
				}
				results := authorizeOnMachine(store, ep, line, dryRun)
				machineFailed := false
				for _, r := range results {
					if r.err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s  FAIL  (%s@%s:%d): %v\n",
							mach.Name, r.user, ep.Host, ep.Port, r.err)
						machineFailed = true
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  (%s@%s:%d)\n",
						mach.Name, r.status, r.user, ep.Host, ep.Port)
				}
				if machineFailed {
					failed = append(failed, mach.Name)
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("add-user: failed on %d machine(s): %s", len(failed), strings.Join(failed, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pubKeyFile, "pubkey", "", "path to a public key file (e.g. ./laptop-two.pub)")
	cmd.Flags().StringVar(&pubKeyLine, "pubkey-line", "", `literal public key line ("ssh-ed25519 AAAA... comment")`)
	cmd.Flags().StringVar(&machine, "machine", "", "restrict to a single manifest machine (default: every machine)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show per-machine action without modifying authorized_keys")
	return cmd
}

// userResult is the per-user outcome of authorizing a pubkey on a
// machine. Exactly one of status/err is set per entry.
type userResult struct {
	user   string
	status string // "present" | "appended" | "would-append"
	err    error
}

// authorizeOnMachine walks every target user on ep.Machine (agent_user
// and bootstrap_user, deduped) and for each: dials as that user with the
// active identity, checks whether the line is already present, and
// appends it if not. Per-user failures do not short-circuit the
// remaining users — one account being unreachable should not block the
// other from getting the key.
//
// Returns one userResult per user attempted, in the order returned by
// targetUsersForMachine (agent first, bootstrap second, root as the
// final fallback when the manifest declared neither).
func authorizeOnMachine(store *state.Store, ep cliutil.Endpoint, line string, dryRun bool) []userResult {
	users := targetUsersForMachine(ep.Machine)
	addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	out := make([]userResult, 0, len(users))
	for _, u := range users {
		out = append(out, authorizeForUser(store, addr, u, line, dryRun))
	}
	return out
}

// authorizeForUser is the single-user worker for authorizeOnMachine.
// Split out so the iteration is trivially inspectable: one dial, one
// verify, one optional append, exactly one return per user.
func authorizeForUser(store *state.Store, addr, user, line string, dryRun bool) userResult {
	r := userResult{user: user}
	client, err := store.DialSSH(addr, user)
	if err != nil {
		r.err = fmt.Errorf("dial %s@%s: %w", user, addr, err)
		return r
	}
	defer client.Close()

	if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(client, line); err == nil {
		r.status = "present"
		return r
	}
	if dryRun {
		r.status = "would-append"
		return r
	}
	if err := sshkeys.AppendAuthorizedKeyLinePOSIX(client, line); err != nil {
		r.err = fmt.Errorf("append: %w", err)
		return r
	}
	r.status = "appended"
	return r
}

// targetUsersForMachine returns the ordered, deduped list of users whose
// authorized_keys `add-user` should touch on this machine. Matches the
// pair that provisioning + ensure-agent-user originally seeded during
// `claws apply`: first the agent user (created post-bootstrap and the
// ongoing-ops identity), then the bootstrap user (still key-accessible
// after `security` because that phase does not disable PermitRootLogin).
// When both fields are empty the conservative default is a single "root"
// entry — the same final fallback preferredUser uses for interactive ssh.
func targetUsersForMachine(m manifestdata.Machine) []string {
	agent := strings.TrimSpace(m.AgentUser)
	bootstrap := strings.TrimSpace(m.BootstrapUser)
	if bootstrap == "" {
		bootstrap = "root"
	}

	seen := make(map[string]bool, 2)
	out := make([]string, 0, 2)
	for _, u := range []string{agent, bootstrap} {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// resolvePubKey returns a cleaned authorized_keys line from flags or, if
// neither flag was given and stdin is a TTY, from an interactive prompt.
// When stdin is not a TTY and neither flag was given, it fails loudly rather
// than hanging a CI pipeline.
func resolvePubKey(flagPath, flagLine string, isTTY bool) (string, error) {
	switch {
	case flagPath != "" && flagLine != "":
		return "", errors.New("--pubkey and --pubkey-line are mutually exclusive")
	case flagPath != "":
		b, err := os.ReadFile(flagPath)
		if err != nil {
			return "", fmt.Errorf("read pubkey %q: %w", flagPath, err)
		}
		return firstNonEmptyLine(string(b))
	case flagLine != "":
		return firstNonEmptyLine(flagLine)
	}
	if !isTTY {
		return "", errors.New("no public key provided: pass --pubkey <file> or --pubkey-line, or run interactively")
	}
	var entered string
	if err := huh.NewInput().
		Title("Public key line").
		Description(`Paste one line, e.g. "ssh-ed25519 AAAA... user@host"`).
		Value(&entered).
		Run(); err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}
	return firstNonEmptyLine(entered)
}

// firstNonEmptyLine returns the first non-blank, non-comment line of s,
// trimmed. Errors if s contains no such line.
func firstNonEmptyLine(s string) (string, error) {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, nil
	}
	return "", errors.New("public key is empty")
}

// parseAuthorizedKeyLine validates via xssh.ParseAuthorizedKey and returns
// the parsed key, the embedded comment (may be empty), and an error if the
// line isn't a syntactically valid OpenSSH public key.
func parseAuthorizedKeyLine(line string) (xssh.PublicKey, string, error) {
	pk, comment, _, _, err := xssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, "", fmt.Errorf("parse public key: %w", err)
	}
	return pk, strings.TrimSpace(comment), nil
}

// selectMachines narrows m.Machines to a single entry (when name != "") or
// returns every machine. Symmetry with resolveMachine's --name handling but
// without the TTY picker: add-user's natural default is "every machine",
// not "pick one".
func selectMachines(m *manifestdata.Manifest, name string) ([]manifestdata.Machine, error) {
	if len(m.Machines) == 0 {
		return nil, errors.New("no machines defined in manifest")
	}
	if name == "" {
		out := make([]manifestdata.Machine, len(m.Machines))
		copy(out, m.Machines)
		return out, nil
	}
	for _, mach := range m.Machines {
		if mach.Name == name {
			return []manifestdata.Machine{mach}, nil
		}
	}
	return nil, fmt.Errorf("no machine named %q in manifest", name)
}

// stdinIsTTY reports whether os.Stdin is an interactive terminal. It exists
// as a package-level hook so tests can force non-TTY behaviour.
var stdinIsTTY = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
