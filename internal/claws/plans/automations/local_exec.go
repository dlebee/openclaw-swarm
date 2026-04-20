package automations

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runLocalOptions tweaks how a local script invocation is wired. Exposed
// for tests — production callers use the zero value, which mirrors the SSH
// path: stdout is discarded and stderr is buffered so it can be folded
// into the returned error on non-zero exit (same contract as
// platformutil/bash.Run, platformutil/python.Run).
type runLocalOptions struct {
	// cmdOverride swaps out the interpreter resolver. Used by tests to
	// redirect to a known-good shim (e.g. capturing stdin into a buffer
	// without requiring bash/python3 on the test runner's PATH).
	cmdOverride func(kind string) (name string, args []string)

	// workingDir sets the child process's cwd. When empty, the child
	// inherits the parent's cwd — which is fine for tests but
	// unpredictable in production, where the operator may invoke claws
	// from anywhere. Production callers set this to the manifest dir so
	// self scripts have a stable, repo-rooted reference point (and
	// symmetric with how execute_file: foo.sh already resolves
	// relative to the manifest).
	workingDir string
}

// runLocal executes script on the operator host via the interpreter
// associated with kind. It is the self-target counterpart to
// DynamicStep.run — same contract (return non-nil err on non-zero exit),
// different transport (os/exec instead of SSH).
//
// Behavior choices:
//
//   - We pipe the script via stdin (same as the SSH path) rather than
//     writing a temp file and exec'ing it: no filesystem side-effects if
//     the script crashes mid-way, and multi-line scripts flow verbatim.
//   - stdout is discarded, stderr is captured and wrapped into the returned
//     error on failure. This matches platformutil/bash.Run / python.Run so
//     self-target steps behave indistinguishably from remote steps at the
//     error-handling layer (Applicable/Check treat any error as "not
//     applicable", Execute/Verify surface it).
//   - Environment variables injected via the step's `env:` allowlist are
//     already baked into the script by bashEnvPreamble / pythonEnvPreamble
//     before runLocal is called. We inherit the parent process env on top
//     of that, so exported names survive.
//
// Unknown kinds fall back to bash; validation rejects truly unknown kinds
// at manifest load, so this is just defense-in-depth for steps constructed
// programmatically.
func runLocal(ctx context.Context, kind, script string, opts runLocalOptions) error {
	name, args := resolveInterpreter(kind, opts.cmdOverride)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Dir = opts.workingDir
	// Expose CLAWS_MANIFEST_DIR so scripts can reference sibling paths
	// without relying on cwd semantics. Scripts that use relative paths
	// still work because cwd is pinned to the manifest dir above — this
	// is just an explicit handle for scripts that prefer absolute
	// resolution. os.Environ() is inherited as usual on top.
	if opts.workingDir != "" {
		cmd.Env = append(os.Environ(), "CLAWS_MANIFEST_DIR="+opts.workingDir)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("self %s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("self %s: %w", name, err)
	}
	return nil
}

func resolveInterpreter(kind string, override func(string) (string, []string)) (string, []string) {
	if override != nil {
		if name, args := override(kind); name != "" {
			return name, args
		}
	}
	if strings.EqualFold(kind, "python") {
		return "python3", []string{"-"}
	}
	return "/bin/bash", []string{"-s"}
}
