package multipass

import (
	"fmt"
	"strings"
)

// buildCloudInit renders a NoCloud user-data document that sets up the
// VM enough for the apply pipeline to SSH in and take over.
//
// It handles three orthogonal concerns, each gated by its own input:
//
//  1. SSH key seeding + optional root access (bootstrapUser).
//     - bootstrapUser == "" or "ubuntu": keys only reach the default
//       cloud-image `ubuntu` user via the top-level
//       `ssh_authorized_keys`.
//     - bootstrapUser == "root": additionally flips `disable_root: false`
//       (cloud-init locks root by default and drops a "login as ubuntu"
//       shim into /root/.ssh/authorized_keys), seeds root's
//       authorized_keys via write_files, and fixes /root/.ssh perms in
//       runcmd (write_files' auto-mkdir uses cloud-init's umask, which
//       is too loose for sshd's StrictModes to accept).
//
//  2. Predictable mDNS hostname (hostname). When set, we:
//     - Override the launch-time hostname with `hostname:` + `fqdn:` so
//       uname -n reports the short, manifest-friendly name (e.g.
//       "gateway-host") instead of the Multipass label
//       ("<prefix>-gateway-host"). Peer VMs can then dial
//       `http://<hostname>.local:8080` from a static manifest field.
//     - Install avahi-daemon + libnss-mdns so other VMs on the same
//       bridge can both resolve AND be resolved via mDNS — Ubuntu cloud
//       images don't ship avahi by default on minimal variants.
//     - Pre-allow UDP 5353 in ufw. ufw is installed but inactive here;
//       `ufw allow` just stages the rule, so once the security phase
//       later runs `ufw --force enable` the mDNS allow is already in
//       the ruleset and multicast survives the hardening.
//
//  3. Header. Multipass uses `#cloud-config` on its own leading line as
//     the datasource discriminator. Always emitted.
//
// All runcmd entries from both the root-path and hostname-path are
// collected into a single `runcmd:` block at the end — YAML would
// otherwise choke on two sibling keys with the same name.
//
// Any bootstrapUser value other than "" / "ubuntu" / "root" is currently
// treated as the "ubuntu" path: the ubuntu account still has passwordless
// sudo, so a custom-named bootstrap user would need a proper cloud-init
// `users:` block to be created. That's outside the scope of the
// multipass integration path today — production deployments run on
// Linode, and Multipass is just for integration tests that use root or
// ubuntu.
func buildCloudInit(keys []string, bootstrapUser, hostname string) string {
	trimmed := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		trimmed = append(trimmed, k)
	}

	hostname = strings.TrimSpace(hostname)
	bootstrapUser = strings.TrimSpace(bootstrapUser)

	var b strings.Builder
	b.WriteString("#cloud-config\n")

	// Hostname first so cloud-init's set_hostname module (early boot)
	// has the value available before any networking / mDNS comes up.
	// manage_etc_hosts keeps /etc/hosts' 127.0.1.1 entry in sync so
	// sudo doesn't emit "unable to resolve host" on every call.
	if hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\n", hostname)
		fmt.Fprintf(&b, "fqdn: %s.local\n", hostname)
		b.WriteString("manage_etc_hosts: true\n")
		b.WriteString("packages:\n")
		b.WriteString("  - avahi-daemon\n")
		b.WriteString("  - libnss-mdns\n")
	}

	// Default-user keys: always. Multipass plays well with the `ubuntu`
	// account (it has passwordless sudo, systemd --user linger support,
	// etc.), so even the root path keeps it authorized as a safety net
	// for debugging.
	b.WriteString("ssh_authorized_keys:\n")
	for _, k := range trimmed {
		fmt.Fprintf(&b, "  - %s\n", k)
	}
	b.WriteString("users:\n  - default\n")

	if bootstrapUser == "root" {
		b.WriteString("disable_root: false\n")
		b.WriteString("write_files:\n")
		b.WriteString("  - path: /root/.ssh/authorized_keys\n")
		b.WriteString("    owner: root:root\n")
		b.WriteString("    permissions: '0600'\n")
		b.WriteString("    content: |\n")
		for _, k := range trimmed {
			fmt.Fprintf(&b, "      %s\n", k)
		}
	}

	// One runcmd block, entries pulled from both feature branches.
	// Order matters: avahi first so peers can start resolving ASAP;
	// ufw allow next so the 5353 rule is staged before security enables
	// ufw; root-dir permission fixups last so they run after any
	// systemd-unit side effects settle.
	var runcmd []string
	if hostname != "" {
		runcmd = append(runcmd,
			"[ systemctl, enable, --now, avahi-daemon ]",
			"[ ufw, allow, 5353/udp, comment, 'mdns' ]",
		)
	}
	if bootstrapUser == "root" {
		runcmd = append(runcmd,
			"[ chmod, '0700', /root/.ssh ]",
			"[ chown, 'root:root', /root/.ssh ]",
		)
	}
	if len(runcmd) > 0 {
		b.WriteString("runcmd:\n")
		for _, c := range runcmd {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}

	return b.String()
}
