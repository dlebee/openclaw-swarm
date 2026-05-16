package remote

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/gluwa/openclaw-swarm2/internal/cli/cliutil"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	"github.com/gluwa/openclaw-swarm2/internal/state"
	"github.com/spf13/cobra"
)

// SSHCmd returns the `claws ssh` command that opens an interactive SSH
// session to a manifest machine. It also hosts related remote operations
// (e.g. `add-user`) as subcommands.
func SSHCmd(manifestFile *string) *cobra.Command {
	var name, user string
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "SSH into a manifest machine",
		Long: strings.TrimSpace(`
Opens an interactive SSH session to one of the machines declared in the manifest.
If only one machine exists it is selected automatically; otherwise an interactive
picker is shown. Use --name to skip the picker.

Subcommands:
  add-user   Authorize an additional SSH public key on every manifest machine.`),
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

			// Resolve the live endpoint (multipass/linode VMs discover
			// their current PublicIPv4 via the hosting provider; ssh-type
			// machines read Host straight from the manifest).
			_, resolver, err := cliutil.PrepareEndpoints(cmd.Context(), abs, m)
			if err != nil {
				return err
			}
			ep, err := resolver.Resolve(*mach)
			if err != nil {
				return err
			}

			// `claws ssh` defaults to the resolver's agent → bootstrap → root
			// precedence, but --user wins when explicitly set: operators
			// may need to re-enter as root on a hardened box before the
			// agent user exists. We override only the user here — the host
			// and port are always what the resolver produced.
			sshUser := strings.TrimSpace(user)
			if sshUser == "" {
				sshUser = ep.User
			}

			keyPath, err := clawssh.ExpandPath(id.PrivateKeyPath)
			if err != nil {
				return fmt.Errorf("resolve key path: %w", err)
			}

			return execSSH(ep.Host, ep.Port, sshUser, keyPath)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "machine name as declared in the manifest")
	cmd.Flags().StringVar(&user, "user", "", "SSH user override (default: agent_user, then bootstrap_user, then root)")
	cmd.AddCommand(AddUserCmd(manifestFile))
	cmd.AddCommand(TestSSHCmd(manifestFile))
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

func resolveManifestPath(manifestFile *string, args []string) (string, error) {
	if manifestFile != nil && strings.TrimSpace(*manifestFile) != "" {
		return *manifestFile, nil
	}
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	return "", fmt.Errorf("specify manifest path: claws -f manifest.yml ssh")
}
