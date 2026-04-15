package ssh

import (
	"fmt"
	"net"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// ClientConfig builds a golang.org/x/crypto/ssh client config from an identity.
// hostKeyCallback defaults to insecure (dev); override for production.
func ClientConfig(id *Identity, user string, hostKeyCallback xssh.HostKeyCallback) (*xssh.ClientConfig, error) {
	if user == "" {
		return nil, fmt.Errorf("ssh user is empty")
	}
	if hostKeyCallback == nil {
		hostKeyCallback = xssh.InsecureIgnoreHostKey()
	}
	signer, err := SignerFromIdentity(id)
	if err != nil {
		return nil, err
	}
	return &xssh.ClientConfig{
		User: user,
		Auth: []xssh.AuthMethod{
			xssh.PublicKeys(signer),
		},
		HostKeyCallback: hostKeyCallback,
		Timeout:         30 * time.Second,
	}, nil
}

// Dial opens an SSH client to addr ("host:port" or "host" with default port 22).
func Dial(network, addr string, id *Identity, user string) (*xssh.Client, error) {
	cc, err := ClientConfig(id, user, nil)
	if err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	return xssh.Dial(network, addr, cc)
}
