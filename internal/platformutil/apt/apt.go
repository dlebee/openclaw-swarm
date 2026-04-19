package apt

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/platformutil/internal/sshutil"
	xssh "golang.org/x/crypto/ssh"
)

// RetryOpts tunes the exponential-backoff behaviour of WithLockRetry.
// Callers rarely need to override these — DefaultRetryOpts is calibrated
// for the real-world apt-daily window on fresh Ubuntu images (tens of
// seconds to a minute or two).
type RetryOpts struct {
	// MaxAttempts is the hard cap on Total attempts, including the first.
	// Zero or negative falls back to DefaultRetryOpts.MaxAttempts.
	MaxAttempts int
	// InitialBackoff is the wait after the first failure.
	InitialBackoff time.Duration
	// MaxBackoff caps the delay after exponential growth.
	MaxBackoff time.Duration
	// Multiplier is the exponential-growth factor. Values <1 fall back to
	// DefaultRetryOpts.Multiplier so "no growth" still makes progress.
	Multiplier float64
	// OnRetry, if non-nil, is invoked before each sleep so callers can log
	// the transient failure (e.g. "apt locked, retrying in 6s…"). attempt
	// is 1-based and counts the attempt that just failed.
	OnRetry func(attempt int, err error, next time.Duration)
}

// DefaultRetryOpts is the retry policy used by Update / Install / RunScript.
// 3s → 6s → 12s → 24s → 30s × 4 = ~2m45s of sleeping across 8 attempts,
// which comfortably outlasts the apt-daily / apt-daily-upgrade window on
// fresh cloud images. The caller's context.Context is the hard deadline —
// a short context will cut retries off earlier.
var DefaultRetryOpts = RetryOpts{
	MaxAttempts:    8,
	InitialBackoff: 3 * time.Second,
	MaxBackoff:     30 * time.Second,
	Multiplier:     2.0,
}

// Update runs `apt-get update` on the remote host with lock-aware retry.
func Update(ctx context.Context, client *xssh.Client) error {
	return RunScript(ctx, client, `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
`)
}

// Install installs one or more packages via apt-get with lock-aware retry.
// Idempotent — re-running with packages already installed is a no-op at
// the apt level.
func Install(ctx context.Context, client *xssh.Client, pkgs ...string) error {
	if len(pkgs) == 0 {
		return fmt.Errorf("apt: no packages specified")
	}
	for _, p := range pkgs {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("apt: empty package name")
		}
	}
	if client == nil {
		return fmt.Errorf("apt: client is nil")
	}
	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get install -y -qq %s
`, strings.Join(pkgs, " "))
	return RunScript(ctx, client, script)
}

// IsInstalled reports whether a package is installed (dpkg status "ii").
// Read-only dpkg query, not gated by the apt lock, so no retry is applied.
func IsInstalled(client *xssh.Client, pkg string) (bool, error) {
	if strings.TrimSpace(pkg) == "" {
		return false, fmt.Errorf("apt: empty package name")
	}
	script := fmt.Sprintf(`set -euo pipefail
dpkg -l %s 2>/dev/null | grep -q '^ii' && echo installed || echo not-installed
`, pkg)
	out, err := sshutil.RunBashStdinOutput(client, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "installed", nil
}

// RunScript executes a bash script that may invoke apt-get, retrying on
// apt/dpkg lock contention per IsLockError. Non-lock failures return
// immediately. Use this when your script embeds apt-get inside a larger
// pipeline (curl | tee, gpg --dearmor, cache-populating one-offs) and you
// want the whole script to retry on lock errors — the script must be
// idempotent, which is typical for the `if ! command -v X; then install X`
// pattern.
func RunScript(ctx context.Context, client *xssh.Client, script string) error {
	if client == nil {
		return fmt.Errorf("apt: client is nil")
	}
	return WithLockRetry(ctx, RetryOpts{}, func() error {
		return sshutil.RunBashStdin(client, script)
	})
}

// RunScriptOutput is the stdout-capturing variant of RunScript. Returns the
// script's stdout on success and the wrapped stderr on failure (same shape
// as sshutil.RunBashStdinOutput).
func RunScriptOutput(ctx context.Context, client *xssh.Client, script string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("apt: client is nil")
	}
	var out string
	err := WithLockRetry(ctx, RetryOpts{}, func() error {
		o, err := sshutil.RunBashStdinOutput(client, script)
		if err != nil {
			return err
		}
		out = o
		return nil
	})
	return out, err
}

// IsLockError reports whether err looks like an apt/dpkg lock contention —
// another apt-get / dpkg / apt-daily process is holding the lock and the
// caller's operation aborted before making progress. Matching is done on
// the error string (case-insensitive) because apt emits these as stderr
// lines and our sshutil helpers embed stderr in the returned error.
//
// Exported so callers that can't route through RunScript (they use their
// own SSH helpers, e.g. common.RunBashOutputWithRetry for transient-
// sshd-restart handling) can integrate the same classification with
// WithLockRetry.
func IsLockError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "could not get lock"):
		return true
	case strings.Contains(s, "unable to lock"):
		return true
	case strings.Contains(s, "unable to acquire the dpkg frontend lock"):
		return true
	case strings.Contains(s, "dpkg frontend is locked"):
		return true
	case strings.Contains(s, "dpkg frontend lock is locked"):
		return true
	case strings.Contains(s, "waiting for cache lock"):
		return true
	case strings.Contains(s, "package cache file is corrupted"):
		// Follow-on symptom of a concurrent apt run stepping on the
		// cache mid-rename. Retrying lets the other apt finish and the
		// next run regenerates the cache cleanly.
		return true
	default:
		return false
	}
}

// WithLockRetry runs op, retrying with exponential backoff while op fails
// with an apt lock error (per IsLockError). It returns the final error
// (wrapped with attempt count) once MaxAttempts is reached, ctx is done,
// or op returns a non-lock error.
//
// A zero-value RetryOpts uses DefaultRetryOpts. Individual fields that are
// zero/negative fall back to their DefaultRetryOpts value, so callers can
// override just one field (e.g. OnRetry for logging) without restating
// the rest.
func WithLockRetry(ctx context.Context, opts RetryOpts, op func() error) error {
	o := mergeRetryOpts(opts)
	var last error
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("apt retry: %w (last: %v)", err, last)
			}
			return err
		}
		last = op()
		if last == nil {
			return nil
		}
		if !IsLockError(last) {
			return last
		}
		if attempt == o.MaxAttempts {
			break
		}
		delay := nextBackoff(attempt, o)
		if o.OnRetry != nil {
			o.OnRetry(attempt, last, delay)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("apt retry: %w (last: %v)", ctx.Err(), last)
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("apt: gave up after %d attempts: %w", o.MaxAttempts, last)
}

func mergeRetryOpts(opts RetryOpts) RetryOpts {
	o := opts
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultRetryOpts.MaxAttempts
	}
	if o.InitialBackoff <= 0 {
		o.InitialBackoff = DefaultRetryOpts.InitialBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultRetryOpts.MaxBackoff
	}
	if o.Multiplier < 1 {
		o.Multiplier = DefaultRetryOpts.Multiplier
	}
	return o
}

// nextBackoff returns the delay before retrying after the Nth failed
// attempt (attempt is 1-based). Growth is geometric capped at MaxBackoff.
func nextBackoff(attempt int, o RetryOpts) time.Duration {
	if attempt <= 1 {
		return o.InitialBackoff
	}
	d := time.Duration(float64(o.InitialBackoff) * math.Pow(o.Multiplier, float64(attempt-1)))
	if d > o.MaxBackoff || d < 0 { // overflow guard
		return o.MaxBackoff
	}
	return d
}
