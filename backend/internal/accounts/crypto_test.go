package accounts

import (
	"testing"
)

func TestCryptoManagerEncryptDecrypt(t *testing.T) {
	cm, err := NewCryptoManager("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Failed to create CryptoManager: %v", err)
	}

	secret := "pionex_api_secret_key_12345"
	encrypted, err := cm.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if encrypted == secret {
		t.Errorf("Encrypted output should not match plain secret")
	}

	decrypted, err := cm.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if decrypted != secret {
		t.Errorf("Expected decrypted %s, got %s", secret, decrypted)
	}
}
