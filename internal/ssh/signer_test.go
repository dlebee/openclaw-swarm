package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

func TestSignerFromPrivateKeyFile_roundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := xssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	signer, err := SignerFromPrivateKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if signer.PublicKey().Type() != "ssh-ed25519" {
		t.Fatalf("key type: %s", signer.PublicKey().Type())
	}
}

func TestSignerFromIdentity_requiresPrivatePath(t *testing.T) {
	_, err := SignerFromIdentity(nil)
	if err == nil {
		t.Fatal("expected error for nil identity")
	}
	_, err = SignerFromIdentity(&Identity{})
	if err == nil {
		t.Fatal("expected error for empty private_key_path")
	}
}
