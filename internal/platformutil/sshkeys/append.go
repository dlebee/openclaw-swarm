package sshkeys

import (
	"encoding/base64"
	"fmt"
	"strings"

	xssh "golang.org/x/crypto/ssh"
)

// AppendAuthorizedKeyLinePOSIX appends pubKey as a single line to $HOME/.ssh/authorized_keys
// for the SSH session user if an identical line is not already present.
// Requires a POSIX Linux remote with /bin/bash and OpenSSH-style paths.
func AppendAuthorizedKeyLinePOSIX(client *xssh.Client, pubKey string) error {
	if client == nil {
		return fmt.Errorf("sshkeys: client is nil")
	}
	line := strings.TrimSpace(pubKey)
	if line == "" {
		return fmt.Errorf("sshkeys: public key is empty")
	}
	return runBashStdin(client, appendAuthorizedKeyScript(line))
}

func appendAuthorizedKeyScript(pubKeyLine string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(pubKeyLine))
	q := "'" + strings.ReplaceAll(enc, "'", "'\\''") + "'"
	return fmt.Sprintf(`set -euo pipefail
KEY_LINE=$(printf '%%s' %s | base64 -d)
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
AUTH="$HOME/.ssh/authorized_keys"
touch "$AUTH"
chmod 600 "$AUTH"
grep -qxF -- "$KEY_LINE" "$AUTH" 2>/dev/null && exit 0
printf '%%s\n' "$KEY_LINE" >> "$AUTH"
`, q)
}
