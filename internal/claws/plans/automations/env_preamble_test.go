package automations

import (
	"strings"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

func TestBashAnsiCQuote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", `$'hello'`},
		{"single-quote", "it's", `$'it\'s'`},
		{"backslash", `a\b`, `$'a\\b'`},
		{"newline", "a\nb", `$'a\nb'`},
		{"tab", "a\tb", `$'a\tb'`},
		{"cr", "a\rb", `$'a\rb'`},
		{"control-byte", "a\x07b", `$'a\x07b'`},
		{"mixed", "pa$$'w\nord", `$'pa$$\'w\nord'`},
		{"empty", "", `$''`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := bashAnsiCQuote(tc.in)
			if got != tc.want {
				t.Fatalf("bashAnsiCQuote(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPythonStringLit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", `"hello"`},
		{"double-quote", `he said "hi"`, `"he said \"hi\""`},
		{"backslash", `a\b`, `"a\\b"`},
		{"newline", "a\nb", `"a\nb"`},
		{"control-byte", "a\x07b", `"a\x07b"`},
		{"empty", "", `""`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pythonStringLit(tc.in)
			if got != tc.want {
				t.Fatalf("pythonStringLit(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBashEnvPreamble(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if got := bashEnvPreamble(nil); got != "" {
			t.Fatalf("empty preamble: got %q, want \"\"", got)
		}
	})
	t.Run("sorted-and-wrapped", func(t *testing.T) {
		t.Parallel()
		// resolveEnvValues sorts for us, but bashEnvPreamble must preserve the
		// incoming order — verify we get the expected wrapping either way.
		pairs := [][2]string{
			{"FOO", "bar"},
			{"TOKEN", "gh'secret"},
		}
		got := bashEnvPreamble(pairs)
		for _, want := range []string{
			"set -a\n",
			`FOO=$'bar'`,
			`TOKEN=$'gh\'secret'`,
			"\nset +a\n",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q in preamble:\n%s", want, got)
			}
		}
	})
}

func TestPythonEnvPreamble(t *testing.T) {
	t.Parallel()
	if got := pythonEnvPreamble(nil); got != "" {
		t.Fatalf("empty preamble: got %q, want \"\"", got)
	}
	pairs := [][2]string{
		{"A", "1"},
		{"MSG", "he said \"hi\""},
	}
	got := pythonEnvPreamble(pairs)
	for _, want := range []string{
		"import os as _ocs_os",
		`    "A": "1",`,
		`    "MSG": "he said \"hi\"",`,
		"del _ocs_os",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in preamble:\n%s", want, got)
		}
	}
}

func TestResolveEnvValues(t *testing.T) {
	t.Parallel()
	resolved := map[string]string{
		"A":     "1",
		"B":     "2",
		"EMPTY": "", // treated as missing by LoadEnvFile callers, but defensively here
	}
	got := resolveEnvValues([]string{"B", "A", "MISSING", "A"}, resolved)
	// Expect sorted, deduped, with MISSING dropped.
	want := [][2]string{{"A", "1"}, {"B", "2"}}
	if len(got) != len(want) {
		t.Fatalf("got %d pairs, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("pair %d: got %v, want %v", i, got[i], w)
		}
	}

	if got := resolveEnvValues(nil, resolved); got != nil {
		t.Fatalf("nil names: got %v, want nil", got)
	}
	if got := resolveEnvValues([]string{"A"}, nil); got != nil {
		t.Fatalf("nil resolved: got %v, want nil", got)
	}
}

func TestDynamicStepEffectiveEnvAllowlist(t *testing.T) {
	t.Parallel()
	t.Run("union-preserves-order-and-dedups", func(t *testing.T) {
		t.Parallel()
		step := manifestdata.AutomationStep{
			Name: "s",
			Env:  []string{"GITHUB_TOKEN", "A"},
		}
		d := NewDynamicStep(step, "", []string{"A", "B", " "}, Options{})
		got := d.effectiveEnvAllowlist()
		want := []string{"A", "B", "GITHUB_TOKEN"}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("empty-returns-nil", func(t *testing.T) {
		t.Parallel()
		d := NewDynamicStep(manifestdata.AutomationStep{}, "", nil, Options{})
		if got := d.effectiveEnvAllowlist(); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
	t.Run("step-only", func(t *testing.T) {
		t.Parallel()
		step := manifestdata.AutomationStep{Env: []string{"X"}}
		d := NewDynamicStep(step, "", nil, Options{})
		got := d.effectiveEnvAllowlist()
		if !equalStrings(got, []string{"X"}) {
			t.Fatalf("got %v, want [X]", got)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
