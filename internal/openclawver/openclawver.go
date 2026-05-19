// Package openclawver parses and compares OpenClaw calendar-version
// strings (e.g. "2026.5.18"). It exists because OpenClaw's
// year.month.patch scheme does NOT sort lexicographically once the
// month or patch crosses ten — `"2026.5.18" < "2026.5.2"` would be
// wrong, which has bitten apply-plan steps that needed a feature gate
// keyed on a specific release (e.g. the node-surface approval flow
// introduced in 2026.5.18). Centralising the parser keeps every
// callsite honest about that and avoids a fleet of one-off regex
// comparisons sprinkled through the codebase.
package openclawver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed OpenClaw calendar version. Patch can be empty
// (zero) for major-line tags like "2026.5"; we treat missing parts as
// 0 so comparison is total.
type Version struct {
	Year  int
	Month int
	Patch int
}

// versionPattern accepts a calendar version anywhere in the input
// string. We use FindStringSubmatch so callers can pass either the
// bare "2026.5.18" or `openclaw --version` output like
// "OpenClaw 2026.5.18 (abc123)" without pre-trimming.
//
// The patch component is required to be present (1–3 digits) so a
// bare "2026.5" string fails parsing — matches the npm publish shape
// (always three numeric segments) and rules out accidental matches in
// log lines that contain a year and month but no patch.
var versionPattern = regexp.MustCompile(`(\d{4})\.(\d{1,2})\.(\d{1,3})`)

// Parse extracts the first calendar-version token from s and returns
// it as a Version. The input may be a bare version string or any
// text containing one (e.g. `openclaw --version` output). Whitespace
// is trimmed from the input; the matched component values must fit
// in standard int — we don't bound them tighter so this also accepts
// hypothetical wide-month or wide-patch releases without breaking.
func Parse(s string) (Version, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Version{}, fmt.Errorf("openclawver: empty version string")
	}
	m := versionPattern.FindStringSubmatch(trimmed)
	if m == nil {
		return Version{}, fmt.Errorf("openclawver: %q is not a calendar version", trimmed)
	}
	year, err := strconv.Atoi(m[1])
	if err != nil {
		return Version{}, fmt.Errorf("openclawver: year %q: %w", m[1], err)
	}
	month, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, fmt.Errorf("openclawver: month %q: %w", m[2], err)
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return Version{}, fmt.Errorf("openclawver: patch %q: %w", m[3], err)
	}
	return Version{Year: year, Month: month, Patch: patch}, nil
}

// MustParse panics if Parse fails. Useful only for package-level
// constants in test code or feature-gate cutoffs whose input is a
// known literal.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String renders the version back to "YYYY.M.P" form (no zero
// padding — matches openclaw's npm tag scheme).
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Year, v.Month, v.Patch)
}

// IsZero reports whether v is the unset zero value. Useful to
// distinguish "no version pinned / probe failed" from a deliberate
// 0.0.0 release (which doesn't exist in practice).
func (v Version) IsZero() bool {
	return v == Version{}
}

// Compare returns -1 / 0 / 1 like bytes.Compare. Sort order is
// year, then month, then patch, all numerically.
func (v Version) Compare(other Version) int {
	if v.Year != other.Year {
		if v.Year < other.Year {
			return -1
		}
		return 1
	}
	if v.Month != other.Month {
		if v.Month < other.Month {
			return -1
		}
		return 1
	}
	if v.Patch != other.Patch {
		if v.Patch < other.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// Less reports whether v sorts before other.
func (v Version) Less(other Version) bool { return v.Compare(other) < 0 }

// AtLeast reports whether v is greater than or equal to other.
// Convention: callers that need "OpenClaw >= X" should write
// `v.AtLeast(X)` so the predicate reads naturally at the call site.
func (v Version) AtLeast(other Version) bool { return v.Compare(other) >= 0 }
