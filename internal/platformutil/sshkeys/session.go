package sshkeys

import (
	"bytes"
	"fmt"
	"strings"

	xssh "golang.org/x/crypto/ssh"
)

func runBashStdin(client *xssh.Client, script string) error {
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
