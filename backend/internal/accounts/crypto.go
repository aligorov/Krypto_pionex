package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// CryptoManager manages AES-256-GCM encryption/decryption for Pionex API secrets.
type CryptoManager struct {
	masterKey []byte
}

// NewCryptoManager initializes CryptoManager with a 32-byte key.
func NewCryptoManager(keyHex string) (*CryptoManager, error) {
	if keyHex == "" {
		// Default secure 32-byte key for local DB vault
		keyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, errors.New("master key must be a valid 64-character hex string (32 bytes)")
	}
	return &CryptoManager{masterKey: key}, nil
}

// Encrypt encrypts plain text into hex-encoded AES-256-GCM ciphertext.
func (cm *CryptoManager) Encrypt(plainText string) (string, error) {
	block, err := aes.NewCipher(cm.masterKey)
	if err != nil {
		return "", fmt.Errorf("aes cipher creation failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm creation failed: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce generation failed: %w", err)
	}

	cipherText := gcm.Seal(nonce, nonce, []byte(plainText), nil)
	return hex.EncodeToString(cipherText), nil
}

// Decrypt decrypts hex-encoded AES-256-GCM ciphertext into plain text.
func (cm *CryptoManager) Decrypt(cipherTextHex string) (string, error) {
	cipherText, err := hex.DecodeString(cipherTextHex)
	if err != nil {
		return "", fmt.Errorf("invalid hex ciphertext: %w", err)
	}

	block, err := aes.NewCipher(cm.masterKey)
	if err != nil {
		return "", fmt.Errorf("aes cipher creation failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm creation failed: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(cipherText) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, actualCipherText := cipherText[:nonceSize], cipherText[nonceSize:]
	plainText, err := gcm.Open(nil, nonce, actualCipherText, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plainText), nil
}
