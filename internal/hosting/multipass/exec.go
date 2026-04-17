package multipass

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner executes the multipass CLI. Splitting this out makes the provider
// unit-testable without a real multipass installation: `fakeRunner` in
// provider_test.go implements Runner with canned stdout / errors.
//
// Contract:
//   - args are passed to the CLI literally; callers are responsible for
//     argument ordering and escaping.
//   - stdin is optional (nil = none); used for `multipass launch --cloud-init -`
//     seed piping.
//   - Stdout is returned on success; stderr is folded into the error message
//     on failure so postmortem has the provider's actual complaint inline
//     rather than only an unhelpful exit status.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout string, err error)
}

// execRunner shells out to the real `multipass` binary on PATH.
type execRunner struct {
	// binary overrides the discovered executable. Empty means "multipass".
	// Kept private because production callers never need to override —
	// tests use a fakeRunner instead.
	binary string
}

// NewExecRunner returns a Runner that invokes the real multipass CLI. If
// multipass is missing from PATH, the first Run call returns a typed error;
// we intentionally do not fail fast here so a consumer can construct a
// Provider at startup and only fail when it tries to use it.
func NewExecRunner() Runner {
	return &execRunner{}
}

func (r *execRunner) Run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	bin := r.binary
	if strings.TrimSpace(bin) == "" {
		bin = "multipass"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w\nstderr: %s",
			bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// IsBinaryAvailable reports whether a `multipass` binary can be resolved on
// PATH. Useful for callers that want to skip (rather than fail) when the CLI
// is absent — most obviously the integration-test preflight.
func IsBinaryAvailable() bool {
	_, err := exec.LookPath("multipass")
	return err == nil
}
