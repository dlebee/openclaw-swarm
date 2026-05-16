package sshfile

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	xssh "golang.org/x/crypto/ssh"
)

// Upload streams the file or directory at localPath on the operator host
// into remotePath on the remote machine. Regular files are copied via
// io.Copy (no full read into memory — suitable for multi-GB binaries).
// Directories recurse; symlinks are followed and written as regular files
// at the destination so the remote side gets a self-contained copy.
//
// If mode != nil it is applied (via sftp.Chmod) to each written file/dir.
// Leaving mode nil keeps the source permissions for files and uses 0755 for
// newly-created parent directories.
//
// The caller owns closing the SSH client. Upload opens a single SFTP
// subsession for the duration of the transfer and closes it on return.
func Upload(client *xssh.Client, localPath, remotePath string, mode *os.FileMode) error {
	if client == nil {
		return fmt.Errorf("sshfile: client is nil")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("sshfile upload: stat local %s: %w", localPath, err)
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("sshfile upload: sftp client: %w", err)
	}
	defer sc.Close()

	if info.IsDir() {
		return uploadDir(sc, localPath, remotePath, mode)
	}
	return uploadFile(sc, localPath, info, remotePath, mode)
}

// Download streams remotePath on the remote machine into localPath on the
// operator host. Regular files are copied via io.Copy; directories recurse.
// See Upload for the mode semantics.
func Download(client *xssh.Client, remotePath, localPath string, mode *os.FileMode) error {
	if client == nil {
		return fmt.Errorf("sshfile: client is nil")
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("sshfile download: sftp client: %w", err)
	}
	defer sc.Close()

	info, err := sc.Stat(remotePath)
	if err != nil {
		return fmt.Errorf("sshfile download: stat remote %s: %w", remotePath, err)
	}
	if info.IsDir() {
		return downloadDir(sc, remotePath, localPath, mode)
	}
	return downloadFile(sc, remotePath, info, localPath, mode)
}

// ---------------------------------------------------------------------------
// Upload helpers
// ---------------------------------------------------------------------------

func uploadFile(sc *sftp.Client, localPath string, info os.FileInfo, remotePath string, mode *os.FileMode) error {
	// Use path.Dir (POSIX, '/'), not filepath.Dir, because remotePath is a
	// remote SFTP path. On Windows operators, filepath.Dir would return
	// backslash-separated text which SFTP would then create as a single
	// literal-backslash directory on the Linux remote.
	if err := sc.MkdirAll(path.Dir(remotePath)); err != nil {
		return fmt.Errorf("sshfile upload: mkdir parent of %s: %w", remotePath, err)
	}
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("sshfile upload: open %s: %w", localPath, err)
	}
	defer src.Close()

	dst, err := sc.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sshfile upload: create %s: %w", remotePath, err)
	}
	// sftp.File.Close reports the final error; defer + named return would be
	// neater but we want the copy error to win if both trip.
	if _, copyErr := io.Copy(dst, src); copyErr != nil {
		_ = dst.Close()
		return fmt.Errorf("sshfile upload: copy to %s: %w", remotePath, copyErr)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("sshfile upload: close %s: %w", remotePath, err)
	}

	applied := applyMode(mode)
	if applied == 0 {
		applied = info.Mode().Perm()
	}
	if err := sc.Chmod(remotePath, applied); err != nil {
		return fmt.Errorf("sshfile upload: chmod %s: %w", remotePath, err)
	}
	return nil
}

func uploadDir(sc *sftp.Client, localDir, remoteDir string, mode *os.FileMode) error {
	if err := sc.MkdirAll(remoteDir); err != nil {
		return fmt.Errorf("sshfile upload: mkdir %s: %w", remoteDir, err)
	}
	return filepath.Walk(localDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(localDir, p)
		if relErr != nil {
			return relErr
		}
		dst := remoteDir
		if rel != "." {
			// sftp paths are always forward-slashed regardless of operator OS.
			dst = remoteDir + "/" + filepath.ToSlash(rel)
		}
		if info.IsDir() {
			if err := sc.MkdirAll(dst); err != nil {
				return fmt.Errorf("sshfile upload: mkdir %s: %w", dst, err)
			}
			return nil
		}
		return uploadFile(sc, p, info, dst, mode)
	})
}

// ---------------------------------------------------------------------------
// Download helpers
// ---------------------------------------------------------------------------

func downloadFile(sc *sftp.Client, remotePath string, info os.FileInfo, localPath string, mode *os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("sshfile download: mkdir parent of %s: %w", localPath, err)
	}
	src, err := sc.Open(remotePath)
	if err != nil {
		return fmt.Errorf("sshfile download: open remote %s: %w", remotePath, err)
	}
	defer src.Close()

	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("sshfile download: create %s: %w", localPath, err)
	}
	if _, copyErr := io.Copy(dst, src); copyErr != nil {
		_ = dst.Close()
		return fmt.Errorf("sshfile download: copy to %s: %w", localPath, copyErr)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("sshfile download: close %s: %w", localPath, err)
	}

	applied := applyMode(mode)
	if applied == 0 {
		applied = info.Mode().Perm()
	}
	if err := os.Chmod(localPath, applied); err != nil {
		return fmt.Errorf("sshfile download: chmod %s: %w", localPath, err)
	}
	return nil
}

func downloadDir(sc *sftp.Client, remoteDir, localDir string, mode *os.FileMode) error {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fmt.Errorf("sshfile download: mkdir %s: %w", localDir, err)
	}
	walker := sc.Walk(remoteDir)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return fmt.Errorf("sshfile download: walk %s: %w", remoteDir, err)
		}
		p := walker.Path()
		info := walker.Stat()
		rel := strings.TrimPrefix(p, remoteDir)
		rel = strings.TrimPrefix(rel, "/")
		dst := localDir
		if rel != "" {
			dst = filepath.Join(localDir, filepath.FromSlash(rel))
		}
		if info.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("sshfile download: mkdir %s: %w", dst, err)
			}
			continue
		}
		if err := downloadFile(sc, p, info, dst, mode); err != nil {
			return err
		}
	}
	return nil
}

// applyMode returns the perm bits to apply, or 0 when no explicit mode was
// requested (the callers fall back to the source's permissions).
func applyMode(mode *os.FileMode) os.FileMode {
	if mode == nil {
		return 0
	}
	return *mode & os.ModePerm
}

