package ssh

import (
	"fmt"
	"os"

	xssh "golang.org/x/crypto/ssh"
)

// SignerFromIdentity loads the private key from id.PrivateKeyPath.
func SignerFromIdentity(id *Identity) (xssh.Signer, error) {
	if id == nil {
		return nil, fmt.Errorf("nil ssh identity")
	}
	if id.PrivateKeyPath == "" {
		return nil, fmt.Errorf("ssh private_key_path is empty")
	}
	path, err := ExpandPath(id.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	return SignerFromPrivateKeyFile(path)
}

// SignerFromPrivateKeyFile reads a PEM/OpenSSH private key from disk.
func SignerFromPrivateKeyFile(path string) (xssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := xssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return signer, nil
}
