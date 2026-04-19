package apt

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestUpdate_nilClient(t *testing.T) {
	if err := Update(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestInstall_nilClient(t *testing.T) {
	if err := Install(context.Background(), nil, "ufw"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestInstall_noPackages(t *testing.T) {
	if err := Install(context.Background(), nil); err == nil {
		t.Fatal("expected error for empty package list")
	}
}

func TestInstall_emptyPackageName(t *testing.T) {
	if err := Install(context.Background(), nil, "ufw", "  "); err == nil {
		t.Fatal("expected error for blank package name")
	}
}

func TestIsInstalled_nilClient(t *testing.T) {
	_, err := IsInstalled(nil, "ufw")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestIsInstalled_emptyPackage(t *testing.T) {
	_, err := IsInstalled(nil, "")
	if err == nil {
		t.Fatal("expected error for empty package name")
	}
}

func TestRunScript_nilClient(t *testing.T) {
	if err := RunScript(context.Background(), nil, `echo hi`); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestRunScriptOutput_nilClient(t *testing.T) {
	if _, err := RunScriptOutput(context.Background(), nil, `echo hi`); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestIsLockError_patterns(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want bool
	}{
		{"nil", nil, false},
		{
			"could-not-get-lock-apt-lists",
			errors.New(`Process exited with status 100: E: Could not get lock /var/lib/apt/lists/lock. It is held by process 3445 (apt-get)
E: Unable to lock directory /var/lib/apt/lists/`),
			true,
		},
		{
			"could-not-get-lock-dpkg-frontend",
			errors.New(`E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 123 (unattended-upgr)`),
			true,
		},
		{
			"dpkg-frontend-locked",
			errors.New(`dpkg: error: dpkg frontend is locked by another process`),
			true,
		},
		{
			"waiting-for-cache-lock",
			errors.New(`Waiting for cache lock: Could not get lock /var/lib/dpkg/lock-frontend.`),
			true,
		},
		{
			"pkgcache-corrupted-followon",
			errors.New(`E: The package cache file is corrupted`),
			true,
		},
		{
			"unrelated-failure",
			errors.New(`E: Unable to locate package nonexistent-pkg`),
			false,
		},
		{
			"ssh-auth-failure",
			errors.New(`ssh: handshake failed: unable to authenticate`),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsLockError(tc.in)
			if got != tc.want {
				t.Fatalf("IsLockError(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestWithLockRetry_succeedsFirstTry(t *testing.T) {
	calls := 0
	err := WithLockRetry(context.Background(), RetryOpts{
		MaxAttempts:    5,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Multiplier:     2,
	}, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestWithLockRetry_retriesOnLockThenSucceeds(t *testing.T) {
	calls := 0
	retries := 0
	err := WithLockRetry(context.Background(), RetryOpts{
		MaxAttempts:    5,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Multiplier:     2,
		OnRetry: func(attempt int, err error, next time.Duration) {
			retries++
		},
	}, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("E: Could not get lock /var/lib/apt/lists/lock")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if retries != 2 {
		t.Fatalf("OnRetry calls = %d, want 2", retries)
	}
}

func TestWithLockRetry_nonLockErrorShortCircuits(t *testing.T) {
	calls := 0
	wantErr := errors.New("E: Unable to locate package foo")
	err := WithLockRetry(context.Background(), RetryOpts{
		MaxAttempts:    5,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Multiplier:     2,
	}, func() error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry for non-lock error)", calls)
	}
}

func TestWithLockRetry_givesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	err := WithLockRetry(context.Background(), RetryOpts{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Multiplier:     2,
	}, func() error {
		calls++
		return errors.New("E: Could not get lock /var/lib/dpkg/lock-frontend")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestWithLockRetry_contextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := WithLockRetry(ctx, RetryOpts{
		MaxAttempts:    5,
		InitialBackoff: time.Millisecond,
	}, func() error {
		calls++
		return errors.New("E: Could not get lock /var/lib/apt/lists/lock")
	})
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 (ctx cancelled before first call)", calls)
	}
}

func TestWithLockRetry_contextDeadlineDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	err := WithLockRetry(ctx, RetryOpts{
		MaxAttempts:    100,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2,
	}, func() error {
		calls++
		return errors.New("E: Could not get lock /var/lib/apt/lists/lock")
	})
	if err == nil {
		t.Fatal("expected ctx deadline error")
	}
	if calls < 1 {
		t.Fatalf("calls = %d, want >=1 (ran at least once)", calls)
	}
}

func TestNextBackoff_caps(t *testing.T) {
	o := RetryOpts{
		InitialBackoff: 2 * time.Second,
		MaxBackoff:     10 * time.Second,
		Multiplier:     2,
	}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 10 * time.Second},
		{5, 10 * time.Second},
		{10, 10 * time.Second},
	}
	for _, tc := range cases {
		got := nextBackoff(tc.attempt, o)
		if got != tc.want {
			t.Errorf("nextBackoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}
