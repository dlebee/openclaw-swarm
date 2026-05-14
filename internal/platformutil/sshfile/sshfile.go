package sshfile

import (
	"fmt"
	"io"
	"os"
	unixpath "path"

	"github.com/pkg/sftp"
	xssh "golang.org/x/crypto/ssh"
)

// ReadFile reads the entire contents of path on the remote host via SFTP.
// Returns os.ErrNotExist when the file does not exist.
func ReadFile(client *xssh.Client, path string) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("sshfile: client is nil")
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("sshfile: sftp client: %w", err)
	}
	defer sc.Close()

	f, err := sc.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("sshfile: open %s: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("sshfile: read %s: %w", path, err)
	}
	return data, nil
}

// WriteFile writes data to path on the remote host via SFTP, creating parent
// directories as needed. The file is created with mode 0644.
func WriteFile(client *xssh.Client, path string, data []byte) error {
	if client == nil {
		return fmt.Errorf("sshfile: client is nil")
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("sshfile: sftp client: %w", err)
	}
	defer sc.Close()

	dir := unixpath.Dir(path)
	if err := sc.MkdirAll(dir); err != nil {
		return fmt.Errorf("sshfile: mkdir %s: %w", dir, err)
	}

	f, err := sc.Create(path)
	if err != nil {
		return fmt.Errorf("sshfile: create %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("sshfile: write %s: %w", path, err)
	}
	return nil
}
