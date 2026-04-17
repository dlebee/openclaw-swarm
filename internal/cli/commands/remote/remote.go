package remote

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/huh"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
)

// SSHCmd returns the `claws ssh` command that opens an interactive SSH
// session to a manifest machine.
func SSHCmd(manifestFile *string) *cobra.Command {
	var name, user string
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "SSH into a manifest machine",
		Long: strings.TrimSpace(`
Opens an interactive SSH session to one of the machines declared in the manifest.
If only one machine exists it is selected automatically; otherwise an interactive
picker is shown. Use --name to skip the picker.`),
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
			id, err := store.ActiveSSHIdentity()
			if err != nil {
				return err
			}

			// `claws ssh` defaults to the agent user — it's the account
			// that stays usable throughout a machine's lifetime. The
			// bootstrap user may have been disabled by security hardening,
			// so falling back to it here would break interactive access on
			// locked-down machines. Operators who need the privileged
			// identity pass --user explicitly.
			sshUser := user
			if sshUser == "" {
				sshUser = strings.TrimSpace(mach.AgentUser)
			}
			if sshUser == "" {
				sshUser = strings.TrimSpace(mach.BootstrapUser)
			}
			if sshUser == "" {
				sshUser = "root"
			}
			port := mach.SSHPort
			if port == 0 {
				port = 22
			}
			host := strings.TrimSpace(mach.Host)
			if host == "" {
				return fmt.Errorf("machine %q has no host", mach.Name)
			}

			return execSSH(host, port, sshUser, id.PrivateKeyPath)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "machine name as declared in the manifest")
	cmd.Flags().StringVar(&user, "user", "", "SSH user override (default: agent_user, then bootstrap_user, then root)")
	return cmd
}

type machineEntry struct {
	Machine manifestdata.Machine
	Roles   []string
}

func resolveMachine(m *manifestdata.Manifest, name string) (*manifestdata.Machine, error) {
	if len(m.Machines) == 0 {
		return nil, fmt.Errorf("no machines defined in manifest")
	}

	entries := buildMachineEntries(m)

	if name != "" {
		for i := range entries {
			if entries[i].Machine.Name == name {
				return &entries[i].Machine, nil
			}
		}
		return nil, fmt.Errorf("no machine named %q in manifest", name)
	}
	if len(entries) == 1 {
		return &entries[0].Machine, nil
	}

	opts := make([]huh.Option[int], len(entries))
	for i, e := range entries {
		label := fmt.Sprintf("%s  (%s)", e.Machine.Name, strings.Join(e.Roles, ", "))
		opts[i] = huh.NewOption(label, i)
	}
	var idx int
	if err := huh.NewSelect[int]().
		Title("Which machine?").
		Options(opts...).
		Value(&idx).
		Run(); err != nil {
		return nil, err
	}
	if idx < 0 || idx >= len(entries) {
		return nil, fmt.Errorf("invalid machine selection")
	}
	return &entries[idx].Machine, nil
}

func buildMachineEntries(m *manifestdata.Manifest) []machineEntry {
	roleMap := make(map[string][]string, len(m.Machines))
	for _, gw := range m.Gateways {
		roleMap[gw.Reference] = append(roleMap[gw.Reference], "gateway:"+gw.Name)
	}
	for _, n := range m.Nodes {
		roleMap[n.Reference] = append(roleMap[n.Reference], "node:"+n.Name)
	}

	entries := make([]machineEntry, 0, len(m.Machines))
	for _, mach := range m.Machines {
		roles := roleMap[mach.Name]
		if len(roles) == 0 {
			roles = []string{"machine"}
		}
		entries = append(entries, machineEntry{Machine: mach, Roles: roles})
	}
	return entries
}

// execSSH replaces the current process with an ssh invocation.
func execSSH(host string, port int, user, keyPath string) error {
	sshBin, err := findSSH()
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	args := []string{
		"ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-p", strconv.Itoa(port),
		fmt.Sprintf("%s@%s", user, host),
	}

	fmt.Fprintf(os.Stderr, "→ ssh %s@%s (port %d)\n", user, addr, port)
	return syscall.Exec(sshBin, args, os.Environ())
}

func findSSH() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, "ssh")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("ssh binary not found in PATH")
}

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	return "", fmt.Errorf("specify manifest path: claws -f manifest.yml ssh")
}
