// Package perflog records optional timing for remote OpenClaw CLI invocations
// (set by the claws CLI --performance-log flag).
package perflog

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	mu sync.Mutex
	w  io.Writer
)

// SetWriter enables logging. Pass nil to disable. Replaces any previous writer
// without closing it.
func SetWriter(out io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	w = out
}

// Close closes the current writer if it implements io.Closer, then disables logging.
func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		return nil
	}
	var err error
	if c, ok := w.(io.Closer); ok {
		err = c.Close()
	}
	w = nil
	return err
}

// Enabled reports whether a log writer is configured.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return w != nil
}

// openclawCLI matches a shell invocation of the openclaw binary (not paths like openclaw.json).
var openclawCLI = regexp.MustCompile(`(?:^|[\s;|&])(openclaw)(?:\s|$|-)`)

// ScriptHasOpenclawCLI reports whether the remote bash script runs the openclaw
// CLI or npm install of the openclaw package.
func ScriptHasOpenclawCLI(script string) bool {
	if openclawCLI.MatchString(script) {
		return true
	}
	s := script
	if strings.Contains(s, "npm") && strings.Contains(s, "install") && strings.Contains(s, "openclaw") {
		return true
	}
	return false
}

// Summarize returns a short, single-line description for the log (first openclaw
// command line or npm install hint).
func Summarize(script string) string {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "openclaw"); i >= 0 {
			s := strings.TrimSpace(line[i:])
			s = strings.ReplaceAll(s, "\t", " ")
			if len(s) > 200 {
				s = s[:200] + "…"
			}
			return s
		}
	}
	if strings.Contains(script, "npm") && strings.Contains(script, "openclaw") {
		return "npm install … openclaw …"
	}
	return "openclaw (script)"
}

// Log writes one line. Safe for concurrent use.
func Log(remote string, summary string, d time.Duration, err error) {
	mu.Lock()
	out := w
	mu.Unlock()
	if out == nil {
		return
	}
	errStr := ""
	if err != nil {
		errStr = err.Error()
		if len(errStr) > 300 {
			errStr = errStr[:300] + "…"
		}
	}
	summary = strings.ReplaceAll(summary, "\t", " ")
	summary = strings.ReplaceAll(summary, "\n", " ")
	// Tab-separated for easy awk; time first for sort.
	fmt.Fprintf(out, "%s\tremote=%s\tduration=%s\tsummary=%s\terr=%q\n",
		time.Now().UTC().Format(time.RFC3339Nano),
		remote,
		d.String(),
		summary,
		errStr,
	)
}
