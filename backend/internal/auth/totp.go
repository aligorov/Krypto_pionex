package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

const (
	totpStepSeconds    = 30
	totpDigits         = 6
	totpSecretBytes    = 20
	recoveryCodeLen    = 8
	recoveryCodesCount = 8
)

var (
	base32Encoder      = base32.StdEncoding.WithPadding(base32.NoPadding)
	ErrInvalidTOTPCode = errors.New("invalid two-factor authentication code")
	ErrTOTPNotEnabled  = errors.New("two-factor authentication is not enabled")
)

// GenerateTOTPSecret creates a random base32 encoded secret key (20 bytes / 160 bits).
func GenerateTOTPSecret() (string, error) {
	bytes := make([]byte, totpSecretBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate random totp secret: %w", err)
	}
	return base32Encoder.EncodeToString(bytes), nil
}

// GenerateRecoveryCodes produces a list of single-use backup recovery codes.
func GenerateRecoveryCodes() ([]string, error) {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // omit easily confused chars: 0, O, 1, I
	codes := make([]string, recoveryCodesCount)
	for i := 0; i < recoveryCodesCount; i++ {
		bytes := make([]byte, recoveryCodeLen)
		if _, err := rand.Read(bytes); err != nil {
			return nil, fmt.Errorf("generate recovery code bytes: %w", err)
		}
		var code strings.Builder
		for j := 0; j < recoveryCodeLen; j++ {
			code.WriteByte(charset[int(bytes[j])%len(charset)])
			if j == 3 {
				code.WriteByte('-')
			}
		}
		codes[i] = code.String()
	}
	return codes, nil
}

// FormatTOTPURL generates the standard otpauth:// URL for QR code scanners.
func FormatTOTPURL(username, secret string) string {
	cleanSecret := strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	label := fmt.Sprintf("Pionex Control:%s", username)
	return fmt.Sprintf(
		"otpauth://totp/%s?secret=%s&issuer=%s&period=%d&digits=%d",
		url.PathEscape(label),
		url.QueryEscape(cleanSecret),
		url.QueryEscape("Pionex Control"),
		totpStepSeconds,
		totpDigits,
	)
}

// GenerateTOTPCode computes the expected 6-digit TOTP code for a given timestamp.
func GenerateTOTPCode(secret string, t time.Time) (string, error) {
	cleanSecret := strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	key, err := base32Encoder.DecodeString(cleanSecret)
	if err != nil {
		// try with standard padded decoder as fallback
		key, err = base32.StdEncoding.DecodeString(cleanSecret)
		if err != nil {
			return "", fmt.Errorf("decode base32 secret: %w", err)
		}
	}

	counter := uint64(t.Unix() / totpStepSeconds)
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(counterBytes[:])
	hash := mac.Sum(nil)

	offset := hash[len(hash)-1] & 0x0f
	binaryCode := binary.BigEndian.Uint32(hash[offset:offset+4]) & 0x7fffffff
	code := binaryCode % uint32(math.Pow10(totpDigits))

	return fmt.Sprintf("%0*d", totpDigits, code), nil
}

// ValidateTOTPCode validates a user-provided 6-digit code with +/- 1 time step tolerance (skew).
func ValidateTOTPCode(secret, userCode string) bool {
	userCode = strings.TrimSpace(userCode)
	if len(userCode) != totpDigits {
		return false
	}

	now := time.Now()
	// Check -1, 0, +1 steps (covering -30s to +30s clock drift)
	for _, delta := range []time.Duration{-totpStepSeconds * time.Second, 0, totpStepSeconds * time.Second} {
		expected, err := GenerateTOTPCode(secret, now.Add(delta))
		if err == nil && subtle.ConstantTimeCompare([]byte(expected), []byte(userCode)) == 1 {
			return true
		}
	}
	return false
}

// NormalizeRecoveryCode strips dashes and uppercase normalizes for comparison.
func NormalizeRecoveryCode(code string) string {
	clean := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	if len(clean) == 8 {
		return clean[:4] + "-" + clean[4:]
	}
	return clean
}
