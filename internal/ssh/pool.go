package ssh

import (
	"context"
	"sync"

	xssh "golang.org/x/crypto/ssh"
)

// DialFunc dials an SSH connection. The pool calls this when no idle client is
// available for the requested host key.
type DialFunc func(ctx context.Context, host string, port int, user string) (*xssh.Client, error)

// Pool is a concurrency-safe pool of SSH clients keyed by host
// (user@host:port). Multiple goroutines may Borrow and Return independently;
// each borrowed client is used by exactly one goroutine at a time.
type Pool struct {
	mu      sync.Mutex
	idle    map[string][]*xssh.Client
	tracked []*xssh.Client // every client ever created, for CloseAll
	closed  bool
}

// NewPool returns an empty pool ready for use.
func NewPool() *Pool {
	return &Pool{idle: make(map[string][]*xssh.Client)}
}

// Borrow returns an idle client for key, or dials a new one via dial.
// The caller must Return the client when done (or let Close clean it up).
func (p *Pool) Borrow(ctx context.Context, key string, dial func(ctx context.Context) (*xssh.Client, error)) (*xssh.Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	if idle := p.idle[key]; len(idle) > 0 {
		c := idle[len(idle)-1]
		p.idle[key] = idle[:len(idle)-1]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.tracked = append(p.tracked, c)
	p.mu.Unlock()
	return c, nil
}

// Return puts a still-healthy client back into the pool for reuse.
// If the pool is already closed the client is closed immediately.
func (p *Pool) Return(key string, c *xssh.Client) {
	if c == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		safeCloseClient(c)
		return
	}
	p.idle[key] = append(p.idle[key], c)
}

// Close closes every client the pool ever handed out (idle or borrowed) and
// prevents future Borrow calls. It implements io.Closer.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	for _, c := range p.tracked {
		safeCloseClient(c)
	}
	p.idle = nil
	p.tracked = nil
	return nil
}

func safeCloseClient(c *xssh.Client) {
	defer func() { recover() }()
	c.Close()
}

// HostKey builds the pool lookup key for a given host endpoint.
func HostKey(host string, port int, user string) string {
	return user + "@" + host + ":" + itoa(port)
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	return string(buf[pos:])
}
