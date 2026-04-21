//go:build !windows

package remote

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func execSSH(host string, port int, user, keyPath string) error {
	sshBin, err := findSSH()
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	args := []string{
		"ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-p", strconv.Itoa(port),
		fmt.Sprintf("%s@%s", user, host),
	}

	fmt.Fprintf(os.Stderr, "→ ssh %s@%s (port %d)\n", user, addr, port)
	return syscall.Exec(sshBin, args, os.Environ())
}

func findSSH() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, "ssh")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("ssh binary not found in PATH")
}
