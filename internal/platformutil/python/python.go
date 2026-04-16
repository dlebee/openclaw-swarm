package python

import (
	"bytes"
	"fmt"
	"strings"

	xssh "golang.org/x/crypto/ssh"
)

// Run executes a Python 3 script on the remote host by piping it to
// python3 via stdin. Returns a non-nil error wrapping stderr on failure.
func Run(client *xssh.Client, script string) error {
	if client == nil {
		return fmt.Errorf("python: client is nil")
	}
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run("python3 -"); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// RunOutput executes a Python 3 script and returns stdout as a string.
func RunOutput(client *xssh.Client, script string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("python: client is nil")
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run("python3 -"); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}
