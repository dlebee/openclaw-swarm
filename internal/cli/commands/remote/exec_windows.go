//go:build windows

package remote

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func execSSH(host string, port int, user, keyPath string) error {
	sshBin, err := findSSH()
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	args := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=NUL",
		"-p", strconv.Itoa(port),
		fmt.Sprintf("%s@%s", user, host),
	}

	fmt.Fprintf(os.Stderr, "→ ssh %s@%s (port %d)\n", user, addr, port)

	cmd := exec.Command(sshBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func findSSH() (string, error) {
	if p, err := exec.LookPath("ssh.exe"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("ssh"); err == nil {
		return p, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		for _, name := range []string{"ssh.exe", "ssh"} {
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("ssh binary not found in PATH (looked for ssh.exe and ssh)")
}
