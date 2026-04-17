package ssh

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

// stubClient returns a throwaway *xssh.Client with a nil conn (good enough for
// pool bookkeeping tests — don't try to run sessions on it).
func stubClient() *xssh.Client {
	return &xssh.Client{}
}

// newTestPool returns a Pool whose liveness probe always reports "alive", so
// stub clients with nil conns can be reused between Borrow calls in tests.
// Individual tests override isAlive when they want to exercise the
// dead-client-discard path.
func newTestPool() *Pool {
	p := NewPool()
	p.isAlive = func(*xssh.Client) bool { return true }
	return p
}

func TestPool_BorrowDialsWhenEmpty(t *testing.T) {
	p := newTestPool()
	defer p.Close()

	var dialed int
	c, err := p.Borrow(context.Background(), "root@10.0.0.1:22", func(ctx context.Context) (*xssh.Client, error) {
		dialed++
		return stubClient(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialed != 1 {
		t.Fatalf("expected 1 dial, got %d", dialed)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestPool_ReturnThenBorrowReuses(t *testing.T) {
	p := newTestPool()
	defer p.Close()

	key := "root@10.0.0.1:22"
	original, _ := p.Borrow(context.Background(), key, func(ctx context.Context) (*xssh.Client, error) {
		return stubClient(), nil
	})
	p.Return(key, original)

	var dialed int
	reused, err := p.Borrow(context.Background(), key, func(ctx context.Context) (*xssh.Client, error) {
		dialed++
		return stubClient(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialed != 0 {
		t.Fatal("expected reuse, but dial was called")
	}
	if reused != original {
		t.Fatal("expected same client back")
	}
}

func TestPool_DifferentKeysAreIndependent(t *testing.T) {
	p := newTestPool()
	defer p.Close()

	a, _ := p.Borrow(context.Background(), "root@10.0.0.1:22", func(ctx context.Context) (*xssh.Client, error) {
		return stubClient(), nil
	})
	p.Return("root@10.0.0.1:22", a)

	var dialed int
	_, err := p.Borrow(context.Background(), "root@10.0.0.2:22", func(ctx context.Context) (*xssh.Client, error) {
		dialed++
		return stubClient(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialed != 1 {
		t.Fatal("different key should trigger a new dial")
	}
}

func TestPool_DialError(t *testing.T) {
	p := newTestPool()
	defer p.Close()

	want := errors.New("connection refused")
	_, err := p.Borrow(context.Background(), "root@10.0.0.1:22", func(ctx context.Context) (*xssh.Client, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestPool_ConcurrentBorrowReturn(t *testing.T) {
	p := newTestPool()
	defer p.Close()

	key := "root@10.0.0.1:22"
	var dials atomic.Int64
	dial := func(ctx context.Context) (*xssh.Client, error) {
		dials.Add(1)
		return stubClient(), nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Borrow(context.Background(), key, dial)
			if err != nil {
				t.Error(err)
				return
			}
			p.Return(key, c)
		}()
	}
	wg.Wait()

	if d := dials.Load(); d > 20 {
		t.Fatalf("too many dials: %d", d)
	}
}

func TestPool_ClosePreventsBorrow(t *testing.T) {
	p := newTestPool()
	p.Close()

	_, err := p.Borrow(context.Background(), "root@10.0.0.1:22", func(ctx context.Context) (*xssh.Client, error) {
		return stubClient(), nil
	})
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestPool_ReturnAfterCloseDoesNotPanic(t *testing.T) {
	p := newTestPool()
	c := stubClient()
	p.Close()
	p.Return("root@10.0.0.1:22", c)
}

func TestPool_ReturnNilIsNoop(t *testing.T) {
	p := newTestPool()
	defer p.Close()
	p.Return("root@10.0.0.1:22", nil)
}

func TestPool_DeadIdleClientIsDiscarded(t *testing.T) {
	p := NewPool()
	defer p.Close()

	// Fail the liveness check exactly once, then start passing. This
	// mimics a pooled connection that went dead while idle (e.g. remote
	// sshd restarted, Tailscale rerouted the default gateway, connection
	// reset by peer, etc.) — Borrow must discard and redial rather than
	// hand the caller a doomed client.
	var probes int
	p.isAlive = func(*xssh.Client) bool {
		probes++
		return probes > 1
	}

	key := "root@10.0.0.1:22"
	dead, _ := p.Borrow(context.Background(), key, func(ctx context.Context) (*xssh.Client, error) {
		return stubClient(), nil
	})
	p.Return(key, dead)

	var dialed int
	fresh, err := p.Borrow(context.Background(), key, func(ctx context.Context) (*xssh.Client, error) {
		dialed++
		return stubClient(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialed != 1 {
		t.Fatalf("expected fresh dial after dead client, got %d dials", dialed)
	}
	if fresh == dead {
		t.Fatal("expected a brand new client, got the dead one back")
	}
}

func TestHostKey(t *testing.T) {
	got := HostKey("10.0.0.1", 22, "root")
	want := "root@10.0.0.1:22"
	if got != want {
		t.Fatalf("HostKey = %q, want %q", got, want)
	}
}
