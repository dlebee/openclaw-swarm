package automations

import (
	"fmt"
	"sort"
	"strings"
)

// resolveEnvValues picks the allowlisted entries out of a resolved env map
// and returns them as a sorted slice of key/value pairs. Sorting gives
// deterministic preamble output (stable hashes, stable dry-run diffs).
// Names not present in the map are silently dropped — the step will just see
// an undefined variable, which matches normal shell semantics.
func resolveEnvValues(names []string, resolved map[string]string) [][2]string {
	if len(names) == 0 || len(resolved) == 0 {
		return nil
	}
	out := make([][2]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		v, ok := resolved[n]
		if !ok {
			continue
		}
		out = append(out, [2]string{n, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// bashEnvPreamble emits a short shell header that exports the given variables
// into the environment of the remainder of the script. Uses `$'...'` ANSI-C
// quoting so values may contain single quotes, newlines, or other shell
// metacharacters safely.
//
// The block is wrapped in `set -a`/`set +a` so simple `VAR=value` assignments
// are automatically exported — this keeps the generated code compact without
// sacrificing the "the rest of the script sees these as env vars" guarantee.
// No other shell state (errexit, etc.) is touched.
func bashEnvPreamble(pairs [][2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# --- openclaw-swarm: injected env (from manifest env_file) ---\n")
	b.WriteString("set -a\n")
	for _, kv := range pairs {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], bashAnsiCQuote(kv[1]))
	}
	b.WriteString("set +a\n")
	b.WriteString("# --- end injected env ---\n")
	return b.String()
}

// pythonEnvPreamble emits a `os.environ.update({...})` block. Values are JSON-
// escaped via pythonStringLit so embedded quotes, backslashes and control
// chars survive intact.
func pythonEnvPreamble(pairs [][2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# --- openclaw-swarm: injected env (from manifest env_file) ---\n")
	b.WriteString("import os as _ocs_os\n")
	b.WriteString("_ocs_os.environ.update({\n")
	for _, kv := range pairs {
		fmt.Fprintf(&b, "    %s: %s,\n", pythonStringLit(kv[0]), pythonStringLit(kv[1]))
	}
	b.WriteString("})\n")
	b.WriteString("del _ocs_os\n")
	b.WriteString("# --- end injected env ---\n")
	return b.String()
}

// bashAnsiCQuote wraps s in bash's `$'...'` ANSI-C quoted form, escaping the
// minimal set of characters that would otherwise terminate the literal or be
// interpreted as a control sequence.
//
// This is safer than plain single-quote wrapping because `$'...'` understands
// backslash escapes (\n, \t, \\, \'), so we can encode ANY value — including
// ones containing single quotes or newlines — without concatenation tricks.
func bashAnsiCQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	b.WriteString("$'")
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				// Other control chars: hex-escape as \xHH (bash $'' understands it).
				fmt.Fprintf(&b, `\x%02x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteString("'")
	return b.String()
}

// pythonStringLit returns a Python string literal for s using double quotes
// and backslash-escaping the characters that would otherwise break the literal
// or introduce ambiguity. This is deliberately hand-rolled rather than
// delegating to e.g. encoding/json so callers don't have to import anything
// to read the output.
func pythonStringLit(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
