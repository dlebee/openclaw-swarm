package sshkeys

import (
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

func TestAppendAuthorizedKeyLinePOSIX_nilClient(t *testing.T) {
	err := AppendAuthorizedKeyLinePOSIX(nil, "ssh-ed25519 AAAAC3")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAppendAuthorizedKeyLinePOSIX_emptyKey(t *testing.T) {
	err := AppendAuthorizedKeyLinePOSIX(&xssh.Client{}, "  \n")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyAuthorizedKeyLinePOSIX_nilClient(t *testing.T) {
	err := VerifyAuthorizedKeyLinePOSIX(nil, "ssh-ed25519 AAAAC3")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyAuthorizedKeyLinePOSIX_emptyKey(t *testing.T) {
	err := VerifyAuthorizedKeyLinePOSIX(&xssh.Client{}, "")
	if err == nil {
		t.Fatal("expected error")
	}
}
