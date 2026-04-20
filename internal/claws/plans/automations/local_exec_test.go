package automations

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunLocal_Bash_Success(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash not guaranteed on windows runners")
	}
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skipf("no /bin/bash available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runLocal(ctx, "bash", "exit 0\n", runLocalOptions{})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestRunLocal_Bash_FailureSurfacesStderr(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash not guaranteed on windows runners")
	}
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skipf("no /bin/bash available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runLocal(ctx, "bash", "echo boom >&2\nexit 7\n", runLocalOptions{})
	if err == nil {
		t.Fatalf("want error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want stderr in error, got %q", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("want *exec.ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 7 {
		t.Fatalf("want exit 7, got %d", exitErr.ExitCode())
	}
}

func TestRunLocal_ContextCancel(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash not guaranteed on windows runners")
	}
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skipf("no /bin/bash available: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := runLocal(ctx, "bash", "sleep 30\n", runLocalOptions{})
	if err == nil {
		t.Fatalf("want context-cancel error")
	}
}

func TestResolveInterpreter(t *testing.T) {
	t.Parallel()
	t.Run("default bash", func(t *testing.T) {
		t.Parallel()
		name, args := resolveInterpreter("", nil)
		if name != "/bin/bash" || len(args) != 1 || args[0] != "-s" {
			t.Fatalf("default: got (%q, %v)", name, args)
		}
	})
	t.Run("python", func(t *testing.T) {
		t.Parallel()
		name, args := resolveInterpreter("python", nil)
		if name != "python3" || len(args) != 1 || args[0] != "-" {
			t.Fatalf("python: got (%q, %v)", name, args)
		}
	})
	t.Run("override wins", func(t *testing.T) {
		t.Parallel()
		name, args := resolveInterpreter("bash", func(k string) (string, []string) {
			return "/usr/local/bin/bash", []string{"-sx"}
		})
		if name != "/usr/local/bin/bash" || args[0] != "-sx" {
			t.Fatalf("override: got (%q, %v)", name, args)
		}
	})
	t.Run("override returning empty falls through", func(t *testing.T) {
		t.Parallel()
		name, _ := resolveInterpreter("bash", func(k string) (string, []string) {
			return "", nil
		})
		if name != "/bin/bash" {
			t.Fatalf("expected fall-through to /bin/bash, got %q", name)
		}
	})
}
