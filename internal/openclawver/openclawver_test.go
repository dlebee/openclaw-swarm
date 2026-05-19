package openclawver

import (
	"strings"
	"testing"
)

func TestParse_bareVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
	}{
		{"2026.5.18", Version{2026, 5, 18}},
		{"2026.5.2", Version{2026, 5, 2}},
		{"2026.4.24", Version{2026, 4, 24}},
		{"2026.12.0", Version{2026, 12, 0}},
		{"2030.1.999", Version{2030, 1, 999}},
		{"  2026.5.18  ", Version{2026, 5, 18}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParse_extractsFromCLIOutput(t *testing.T) {
	// `openclaw --version` shape on 2026.5.x is "OpenClaw 2026.5.18 (abc123)".
	// Older builds also accept e.g. "OpenClaw v2026.5.2" — version pattern is
	// not anchored, so we extract whichever calendar version appears first.
	cases := []struct {
		in   string
		want Version
	}{
		{"OpenClaw 2026.5.18 (8b2a6e5)", Version{2026, 5, 18}},
		{"OpenClaw 2026.5.12 (f066dd2)\n", Version{2026, 5, 12}},
		{"openclaw v2026.4.24-rc1", Version{2026, 4, 24}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParse_rejectsNonCalver(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"openclaw",
		"v1.2",                // missing patch
		"v1.2.3.4",            // 4-segment is rejected: year must be 4 digits and we only look at first 3
		"2026/05/18",          // wrong delimiter
		"hello world",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := Parse(in)
			if err == nil {
				t.Fatalf("Parse(%q) expected error, got nil", in)
			}
			if in != "" && in != "   " && !strings.Contains(err.Error(), "openclawver:") {
				t.Fatalf("error %q missing package prefix", err.Error())
			}
		})
	}
}

func TestCompare_orderHandlesDoubleDigitMonthAndPatch(t *testing.T) {
	// The regression that motivated this package: lexicographic compare
	// would say "2026.5.18" < "2026.5.2" because '1' < '2'. The numeric
	// compare must put .18 strictly after .2.
	v18 := MustParse("2026.5.18")
	v2 := MustParse("2026.5.2")
	if !v2.Less(v18) {
		t.Fatalf("2026.5.2 should be Less than 2026.5.18, got Less=false")
	}
	if v18.Less(v2) {
		t.Fatalf("2026.5.18 must not be Less than 2026.5.2")
	}
	if c := v18.Compare(v2); c <= 0 {
		t.Fatalf("Compare(2026.5.18, 2026.5.2) = %d, want > 0", c)
	}

	// Same logic across months: 2026.10.0 vs 2026.5.999.
	a := MustParse("2026.10.0")
	b := MustParse("2026.5.999")
	if !b.Less(a) {
		t.Fatalf("2026.5.999 should be Less than 2026.10.0 (month dominates patch)")
	}

	// And across years.
	old := MustParse("2024.12.99")
	newer := MustParse("2025.1.0")
	if !old.Less(newer) {
		t.Fatalf("year dominates: 2024.12.99 must be Less than 2025.1.0")
	}
}

func TestCompare_equalVersions(t *testing.T) {
	a := MustParse("2026.5.18")
	b := MustParse("2026.5.18")
	if a.Compare(b) != 0 {
		t.Fatalf("equal versions must Compare to 0, got %d", a.Compare(b))
	}
	if a.Less(b) || b.Less(a) {
		t.Fatalf("equal versions must not be Less than each other")
	}
	if !a.AtLeast(b) || !b.AtLeast(a) {
		t.Fatalf("equal versions must each AtLeast the other")
	}
}

func TestAtLeast_predicateReadsCorrectly(t *testing.T) {
	cutoff := MustParse("2026.5.18")
	cases := []struct {
		in      string
		atLeast bool
	}{
		{"2026.5.18", true}, // exact match
		{"2026.5.19", true},
		{"2026.6.0", true},
		{"2027.1.0", true},
		{"2026.5.17", false},
		{"2026.5.2", false},
		{"2026.4.24", false},
		{"2025.12.99", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			v := MustParse(tc.in)
			got := v.AtLeast(cutoff)
			if got != tc.atLeast {
				t.Fatalf("%s.AtLeast(%s) = %v, want %v", tc.in, cutoff, got, tc.atLeast)
			}
		})
	}
}

func TestString_roundtripsParse(t *testing.T) {
	v := MustParse("2026.5.18")
	if v.String() != "2026.5.18" {
		t.Fatalf("String() = %q, want 2026.5.18", v.String())
	}
	again, err := Parse(v.String())
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if again != v {
		t.Fatalf("roundtrip mismatch: %+v != %+v", again, v)
	}
}

func TestIsZero(t *testing.T) {
	var zero Version
	if !zero.IsZero() {
		t.Fatalf("default Version must be IsZero")
	}
	if (Version{2026, 5, 18}).IsZero() {
		t.Fatalf("2026.5.18 must not be IsZero")
	}
}

func TestMustParse_panicsOnInvalid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("MustParse(\"garbage\") must panic")
		}
	}()
	_ = MustParse("garbage")
}
