package multipass

import (
	"fmt"
	"strings"
)

// buildCloudInit renders a minimal NoCloud user-data document that:
//
//   - ensures every public key in `keys` is installed under the default
//     `ubuntu` user's authorized_keys
//   - leaves apt/packages/cloud-init-managed files alone: the provisioning
//     phase will continue over SSH once cloud-init finishes, and we want
//     that code path to be identical to Linode's
//
// A `root_pass` is deliberately NOT set — Multipass doesn't expose
// `authorized_keys` wiring for root via cloud-init consistently across
// versions, and the scaffold plan's downstream security phase locks root
// SSH anyway. The ephemeral test key going onto `ubuntu` is enough for
// our purposes (including `sudo` for step scripts, since cloud-image
// `ubuntu` already has passwordless sudo).
//
// The document is prefixed with `#cloud-config` on its own line; Multipass
// treats that header as the datasource discriminator.
func buildCloudInit(keys []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_authorized_keys:\n")
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		// Single-quoted YAML scalar; keys may contain spaces but never ''
		// since they're base64 + ASCII labels.
		fmt.Fprintf(&b, "  - %s\n", k)
	}
	b.WriteString("users:\n  - default\n")
	return b.String()
}
