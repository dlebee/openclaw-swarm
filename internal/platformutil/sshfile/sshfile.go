package sshfile

import (
	"fmt"
	"io"
	"os"
	unixpath "path"

	"github.com/pkg/sftp"
	xssh "golang.org/x/crypto/ssh"
)

// ReadFile reads the entire contents of remotePath on the remote host via SFTP.
// Returns os.ErrNotExist when the file does not exist. remotePath MUST be a
// POSIX path (forward slashes) — see WriteFile for why.
func ReadFile(client *xssh.Client, remotePath string) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("sshfile: client is nil")
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("sshfile: sftp client: %w", err)
	}
	defer sc.Close()

	f, err := sc.Open(remotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("sshfile: open %s: %w", remotePath, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("sshfile: read %s: %w", remotePath, err)
	}
	return data, nil
}

// WriteFile writes data to remotePath on the remote host via SFTP, creating
// parent directories as needed. The file is created with mode 0644.
//
// remotePath MUST be a POSIX path (forward slashes). We use path.Dir — not
// filepath.Dir — because filepath uses the host OS separator, which means a
// Windows-side caller producing /home/x/y/z would get \home\x\y\z back from
// filepath.Dir and SFTP would happily MkdirAll a single literal-backslash
// directory on the Linux remote. Always use path.* for remote paths.
func WriteFile(client *xssh.Client, remotePath string, data []byte) error {
	if client == nil {
		return fmt.Errorf("sshfile: client is nil")
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("sshfile: sftp client: %w", err)
	}
	defer sc.Close()

	dir := unixpath.Dir(remotePath)
	if err := sc.MkdirAll(dir); err != nil {
		return fmt.Errorf("sshfile: mkdir %s: %w", dir, err)
	}

	f, err := sc.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sshfile: create %s: %w", remotePath, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("sshfile: write %s: %w", remotePath, err)
	}
	return nil
}
