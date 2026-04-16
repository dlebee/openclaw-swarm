package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	clawssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// Host identifies a remote machine for SSH operations.
type Host struct {
	Addr string
	Port int
	User string
}

func (h Host) String() string {
	return fmt.Sprintf("%s@%s:%d", h.User, h.Addr, h.Port)
}

// Service provides read-only OpenClaw probing via the CLI's --json interface.
type Service struct {
	pool *clawssh.Pool
	dial clawssh.DialFunc
}

// New creates a Service backed by the given pool and dial function.
func New(pool *clawssh.Pool, dial clawssh.DialFunc) *Service {
	return &Service{pool: pool, dial: dial}
}

func (s *Service) conn(ctx context.Context, h Host) (*xssh.Client, string, error) {
	key := clawssh.HostKey(h.Addr, h.Port, h.User)
	c, err := s.pool.Borrow(ctx, key, func(ctx context.Context) (*xssh.Client, error) {
		return s.dial(ctx, h.Addr, h.Port, h.User)
	})
	return c, key, err
}

// shellOutput borrows a client, executes a bash script, and returns stdout.
func (s *Service) shellOutput(ctx context.Context, h Host, script string) (string, error) {
	c, key, err := s.conn(ctx, h)
	if err != nil {
		return "", err
	}
	out, err := bash.RunOutput(c, script)
	s.pool.Return(key, c)
	return out, err
}

// shellJSON executes a script on the remote host and unmarshals the JSON
// stdout into T. This is the single entry-point for all typed --json reads.
func shellJSON[T any](ctx context.Context, s *Service, h Host, script string) (T, error) {
	var zero T
	out, err := s.shellOutput(ctx, h, script)
	if err != nil {
		return zero, err
	}
	var result T
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &result); err != nil {
		return zero, fmt.Errorf("parse json: %w", err)
	}
	return result, nil
}

// readFile reads a file from the remote host via SFTP.
func (s *Service) readFile(ctx context.Context, h Host, path string) ([]byte, error) {
	c, key, err := s.conn(ctx, h)
	if err != nil {
		return nil, err
	}
	data, err := sshfile.ReadFile(c, path)
	s.pool.Return(key, c)
	return data, err
}
