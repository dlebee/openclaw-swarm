package mesh

import (
	"strings"
	"unicode"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	xssh "golang.org/x/crypto/ssh"
)

// Plan cache keys shared across mesh steps.
const (
	CacheKeyControlURL = "mesh.controlURL"
	CacheKeyPreauthKey = "mesh.preauthKey"
)

const headscaleVersion = "0.28.0"

// TailscaleIP reads the node's Tailscale IPv4 address. Empty string if not joined.
func TailscaleIP(client *xssh.Client) string {
	out, err := bash.RunOutput(client, `command -v tailscale >/dev/null 2>&1 && tailscale ip -4 2>/dev/null || true`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// HeadscaleInstalled reports whether the headscale binary and config exist.
func HeadscaleInstalled(client *xssh.Client) bool {
	out, err := bash.RunOutput(client, `test -x /usr/local/bin/headscale && test -f /etc/headscale/config.yaml && echo ok || echo no`)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "ok"
}

// HeadscaleVersionOK reports whether the installed headscale matches the pinned version.
func HeadscaleVersionOK(client *xssh.Client) bool {
	out, err := bash.RunOutput(client, `/usr/local/bin/headscale version 2>/dev/null || true`)
	if err != nil {
		return false
	}
	return strings.Contains(out, headscaleVersion)
}

// IsHTTPControlURL returns true if the control URL uses plain HTTP (no Caddy needed).
func IsHTTPControlURL(controlURL string) bool {
	return strings.HasPrefix(strings.TrimSpace(controlURL), "http://")
}

// HostnameFromControlURL extracts the FQDN from a control URL (stripping scheme and port).
func HostnameFromControlURL(controlURL string) string {
	u := strings.TrimSpace(controlURL)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if idx := strings.IndexByte(u, '/'); idx >= 0 {
		u = u[:idx]
	}
	if idx := strings.IndexByte(u, ':'); idx >= 0 {
		u = u[:idx]
	}
	return u
}

// SanitizeDNSLabel maps a gateway name to a single DNS label (lowercase, hyphens).
func SanitizeDNSLabel(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" {
		return "gw"
	}
	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}
	return s
}
