package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

// A syntactically valid Ed25519 public key line for parsing tests. Picked
// so the base64 decodes to the right ssh-ed25519 wire format; no corresponding
// private key exists anywhere.
const validEd25519Line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFbGZbGcr3yn+p0wZ0J3XhIVwHcEi7rU1OMSaf0PLUhQ laptop-two"

func TestFirstNonEmptyLine(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "simple", in: "ssh-ed25519 AAAA key", want: "ssh-ed25519 AAAA key"},
		{name: "leading blanks", in: "\n\n  \nssh-ed25519 AAAA key\n", want: "ssh-ed25519 AAAA key"},
		{name: "comment skipped", in: "# comment\nssh-ed25519 AAAA key\n", want: "ssh-ed25519 AAAA key"},
		{name: "crlf trimmed", in: "ssh-ed25519 AAAA key\r\n", want: "ssh-ed25519 AAAA key"},
		{name: "empty", in: "   \n\n", wantErr: true},
		{name: "only comments", in: "# a\n# b\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := firstNonEmptyLine(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseAuthorizedKeyLine(t *testing.T) {
	t.Run("valid has comment", func(t *testing.T) {
		pk, comment, err := parseAuthorizedKeyLine(validEd25519Line)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if pk == nil {
			t.Fatal("expected non-nil pubkey")
		}
		if comment != "laptop-two" {
			t.Fatalf("comment = %q, want %q", comment, "laptop-two")
		}
	})
	t.Run("garbage rejected", func(t *testing.T) {
		if _, _, err := parseAuthorizedKeyLine("not a real key"); err == nil {
			t.Fatal("expected error for garbage input")
		}
	})
	t.Run("empty rejected", func(t *testing.T) {
		if _, _, err := parseAuthorizedKeyLine(""); err == nil {
			t.Fatal("expected error for empty input")
		}
	})
}

func TestResolvePubKey(t *testing.T) {
	tmp := t.TempDir()
	good := filepath.Join(tmp, "good.pub")
	if err := os.WriteFile(good, []byte(validEd25519Line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		path    string
		line    string
		isTTY   bool
		wantErr string // substring; "" means no error
		want    string
	}{
		{
			name: "flag file happy path",
			path: good,
			want: validEd25519Line,
		},
		{
			name: "flag line happy path",
			line: "  " + validEd25519Line + "  ",
			want: validEd25519Line,
		},
		{
			name:    "both flags conflict",
			path:    good,
			line:    validEd25519Line,
			wantErr: "mutually exclusive",
		},
		{
			name:    "missing file surfaces error",
			path:    filepath.Join(tmp, "missing.pub"),
			wantErr: "read pubkey",
		},
		{
			name:    "no flags non-tty fails loudly",
			isTTY:   false,
			wantErr: "no public key provided",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePubKey(tc.path, tc.line, tc.isTTY)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTargetUsersForMachine(t *testing.T) {
	cases := []struct {
		name string
		m    manifestdata.Machine
		want []string
	}{
		{
			name: "agent + distinct bootstrap → both in agent-first order",
			m:    manifestdata.Machine{AgentUser: "agent", BootstrapUser: "root"},
			want: []string{"agent", "root"},
		},
		{
			name: "agent only → agent + root fallback for bootstrap",
			m:    manifestdata.Machine{AgentUser: "agent"},
			want: []string{"agent", "root"},
		},
		{
			name: "bootstrap only → just bootstrap",
			m:    manifestdata.Machine{BootstrapUser: "root"},
			want: []string{"root"},
		},
		{
			name: "agent equals bootstrap → deduped to one entry",
			m:    manifestdata.Machine{AgentUser: "ops", BootstrapUser: "ops"},
			want: []string{"ops"},
		},
		{
			name: "whitespace trimmed before compare",
			m:    manifestdata.Machine{AgentUser: "  agent ", BootstrapUser: "agent"},
			want: []string{"agent"},
		},
		{
			name: "both empty → root fallback",
			m:    manifestdata.Machine{},
			want: []string{"root"},
		},
		{
			name: "both whitespace → root fallback",
			m:    manifestdata.Machine{AgentUser: "   ", BootstrapUser: "\t"},
			want: []string{"root"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := targetUsersForMachine(tc.m)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d] = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestSelectMachines(t *testing.T) {
	m := &manifestdata.Manifest{
		Machines: []manifestdata.Machine{
			{Name: "a"},
			{Name: "b"},
			{Name: "c"},
		},
	}

	t.Run("all machines", func(t *testing.T) {
		got, err := selectMachines(m, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("want 3, got %d", len(got))
		}
	})
	t.Run("single by name", func(t *testing.T) {
		got, err := selectMachines(m, "b")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "b" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("unknown name errors", func(t *testing.T) {
		if _, err := selectMachines(m, "nope"); err == nil {
			t.Fatal("expected error for unknown machine")
		}
	})
	t.Run("empty manifest errors", func(t *testing.T) {
		if _, err := selectMachines(&manifestdata.Manifest{}, ""); err == nil {
			t.Fatal("expected error for empty manifest")
		}
	})
}
