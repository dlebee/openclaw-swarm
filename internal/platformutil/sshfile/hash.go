package sshfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	xssh "golang.org/x/crypto/ssh"
)

// LocalSHA256 computes the SHA256 of a regular file on the operator host
// and returns it as a lowercase hex string. Directories and symlinks to
// directories are rejected — the caller is expected to have checked the
// shape already (we want hashing to be O(1) allocations and have exactly
// one meaning). os.ErrNotExist is returned verbatim when the path is
// missing so callers can distinguish "absent" from "other IO error".
func LocalSHA256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("sshfile: LocalSHA256 on directory %s is not supported", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sshfile: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RemoteSHA256 computes the SHA256 of a regular file on the target via
// `sha256sum`. Returns (hex, exists=true, nil) on success, ("",
// exists=false, nil) when the file does not exist, and ("", false, err)
// for any other failure (missing sha256sum binary, permission denied on
// a parent dir, transport hiccup, …). Distinguishing "absent" from
// "errored" matters because callers use this in a Check predicate where
// absent → unsatisfied and error → surface to the user.
//
// The script is intentionally a single pipeline: `stat` decides the
// branch, sha256sum does the work. Doing it remote-side in one round trip
// avoids a second SSH exec for the existence probe.
func RemoteSHA256(client *xssh.Client, path string) (sum string, exists bool, err error) {
	if client == nil {
		return "", false, errors.New("sshfile: client is nil")
	}
	// Uses printf so "MISSING"/"EXISTS" markers can't collide with a real
	// sha256 hex string. Any sha256sum tool found (coreutils or busybox)
	// emits "<hex>  <path>" on stdout; we take field 1.
	script := fmt.Sprintf(`if [ ! -e %[1]s ]; then
  printf 'MISSING\n'
  exit 0
fi
if [ -d %[1]s ]; then
  printf 'ISDIR\n'
  exit 0
fi
sha256sum -- %[1]s | awk '{print $1}'`, shellQuote(path))

	out, runErr := bash.RunOutput(client, script)
	if runErr != nil {
		return "", false, fmt.Errorf("sshfile: sha256sum %s: %w", path, runErr)
	}
	trimmed := strings.TrimSpace(out)
	switch trimmed {
	case "":
		return "", false, fmt.Errorf("sshfile: sha256sum %s: empty output", path)
	case "MISSING":
		return "", false, nil
	case "ISDIR":
		return "", false, fmt.Errorf("sshfile: RemoteSHA256 on directory %s is not supported", path)
	}
	// Light sanity check: a sha256 hex is 64 chars of [0-9a-f]. If the
	// remote spewed something else (e.g. a setup-wide MOTD bleeding into
	// stdout) we'd rather error than silently "not equal" on every run.
	if len(trimmed) != 64 || !isHex(trimmed) {
		return "", false, fmt.Errorf("sshfile: sha256sum %s: unexpected output %q", path, trimmed)
	}
	return trimmed, true, nil
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
// Good enough for absolute paths passed to a remote shell — we're not
// accepting user-supplied command arguments here, just file paths from
// the manifest.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
