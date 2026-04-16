package bash

import "testing"

func TestRun_nilClient(t *testing.T) {
	if err := Run(nil, "echo hi"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestRunOutput_nilClient(t *testing.T) {
	_, err := RunOutput(nil, "echo hi")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}
