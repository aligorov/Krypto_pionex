package accounts

import (
	"bytes"
	"testing"
)

func TestCredentialEncryptionRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	nonceBytes := bytes.Repeat([]byte{0x11}, 64)
	ciphertext, err := encrypt(key, "api-key", "sensitive-value", bytes.NewReader(nonceBytes))
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if ciphertext == "sensitive-value" || ciphertext == "" {
		t.Fatal("credential was not encrypted")
	}
	plaintext, err := decrypt(key, "api-key", ciphertext)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if plaintext != "sensitive-value" {
		t.Fatalf("unexpected plaintext %q", plaintext)
	}
	if _, err := decrypt(key, "api-secret", ciphertext); err == nil {
		t.Fatal("ciphertext must be bound to its purpose")
	}
}
