package node

import "testing"

// TestParseExecPolicyJSON pins the regression where the exec-policy JSON
// parser read the wrong path and always returned ok=false, forcing a
// fallback to `config get tools.exec.security` that reads back empty on
// 2026.8.1 — surfacing as `exec-policy verify: security "", want "full"`.
//
// The realShape fixture is the verbatim `openclaw exec-policy show --json`
// output captured on 2026.7.1 (the 8.1 shape is the same nested
// effectivePolicy.scopes[] layout).
func TestParseExecPolicyJSON(t *testing.T) {
	realShape := `{"configPath":"/root/.openclaw/openclaw.json","approvalsPath":"/root/.openclaw/exec-approvals.json","approvalsExists":true,"effectivePolicy":{"note":"Effective exec policy is the host approvals file intersected with requested tools.exec policy.","scopes":[{"scopeLabel":"tools.exec","configPath":"tools.exec","host":{"requested":"auto","requestedSource":"OpenClaw default (auto)"},"mode":{"requested":"full","effective":"full"},"security":{"requested":"full","requestedSource":"OpenClaw default (full)","host":"full","effective":"full"},"ask":{"requested":"off","host":"off","effective":"off"},"askFallback":{"effective":"deny"},"runtimeApprovalsSource":"local-file"}]}}`

	tests := []struct {
		name    string
		in      string
		wantSec string
		wantAsk string
		wantOK  bool
	}{
		{
			name:    "real 2026.x nested effectivePolicy shape",
			in:      realShape,
			wantSec: "full",
			wantAsk: "off",
			wantOK:  true,
		},
		{
			name:    "leading noise before json is skipped",
			in:      "warning: something\n" + realShape,
			wantSec: "full",
			wantAsk: "off",
			wantOK:  true,
		},
		{
			name: "requested empty falls back to effective",
			in: `{"effectivePolicy":{"scopes":[{"configPath":"tools.exec",` +
				`"security":{"effective":"full"},"ask":{"effective":"off"}}]}}`,
			wantSec: "full",
			wantAsk: "off",
			wantOK:  true,
		},
		{
			name: "non-exec scope ignored, tools.exec selected",
			in: `{"effectivePolicy":{"scopes":[` +
				`{"configPath":"tools.other","security":{"requested":"limited"}},` +
				`{"configPath":"tools.exec","security":{"requested":"full"},"ask":{"requested":"off"}}]}}`,
			wantSec: "full",
			wantAsk: "off",
			wantOK:  true,
		},
		{
			name:   "empty security -> not ok (caller falls back)",
			in:     `{"effectivePolicy":{"scopes":[{"configPath":"tools.exec","security":{},"ask":{}}]}}`,
			wantOK: false,
		},
		{
			name:   "no scopes -> not ok",
			in:     `{"effectivePolicy":{"scopes":[]}}`,
			wantOK: false,
		},
		{
			name:   "not json -> not ok",
			in:     `Unknown config path`,
			wantOK: false,
		},
		{
			name:   "empty -> not ok",
			in:     ``,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sec, ask, ok := parseExecPolicyJSON(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (sec=%q ask=%q)", ok, tt.wantOK, sec, ask)
			}
			if !tt.wantOK {
				return
			}
			if sec != tt.wantSec {
				t.Errorf("security = %q, want %q", sec, tt.wantSec)
			}
			if ask != tt.wantAsk {
				t.Errorf("ask = %q, want %q", ask, tt.wantAsk)
			}
		})
	}
}
