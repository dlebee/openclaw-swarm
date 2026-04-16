// Package sshutil provides shared SSH session helpers for platformutil packages.
package sshutil

import (
	"bytes"
	"fmt"
	"strings"

	xssh "golang.org/x/crypto/ssh"
)

// RunBashStdin executes a bash script on the remote host by piping it to
// /bin/bash -s via stdin. Returns a non-nil error wrapping stderr on failure.
func RunBashStdin(client *xssh.Client, script string) error {
	if client == nil {
		return fmt.Errorf("sshutil: client is nil")
	}
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run("/bin/bash -s"); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// RunBashStdinOutput executes a bash script and returns combined stdout.
func RunBashStdinOutput(client *xssh.Client, script string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("sshutil: client is nil")
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
	if err := sess.Run("/bin/bash -s"); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}
