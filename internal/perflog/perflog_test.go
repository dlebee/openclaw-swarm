package perflog

import "testing"

func TestScriptHasOpenclawCLI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"openclaw with flag", "openclaw agents list --json", true},
		{"after preamble", "export X=1\nopenclaw config get foo", true},
		{"npm install", "sudo npm install -g openclaw --quiet", true},
		{"path only", "cat ~/.openclaw/config.json", false},
		{"openclaw.json path", "mv openclaw.json /tmp/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScriptHasOpenclawCLI(tt.in); got != tt.want {
				t.Errorf("ScriptHasOpenclawCLI() = %v, want %v", got, tt.want)
			}
		})
	}
}
