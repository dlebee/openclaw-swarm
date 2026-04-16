package sshkeys

import (
	"encoding/base64"
	"fmt"
	"strings"

	xssh "golang.org/x/crypto/ssh"
)

// VerifyAuthorizedKeyLinePOSIX returns nil if pubKey appears as a full line in
// $HOME/.ssh/authorized_keys for the SSH session user.
// Requires a POSIX Linux remote with /bin/bash and OpenSSH-style paths.
func VerifyAuthorizedKeyLinePOSIX(client *xssh.Client, pubKey string) error {
	if client == nil {
		return fmt.Errorf("sshkeys: client is nil")
	}
	line := strings.TrimSpace(pubKey)
	if line == "" {
		return fmt.Errorf("sshkeys: public key is empty")
	}
	return runBashStdin(client, verifyAuthorizedKeyScript(line))
}

func verifyAuthorizedKeyScript(pubKeyLine string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(pubKeyLine))
	q := "'" + strings.ReplaceAll(enc, "'", "'\\''") + "'"
	return fmt.Sprintf(`set -euo pipefail
KEY_LINE=$(printf '%%s' %s | base64 -d)
AUTH="$HOME/.ssh/authorized_keys"
test -f "$AUTH"
grep -qxF -- "$KEY_LINE" "$AUTH"
`, q)
}
