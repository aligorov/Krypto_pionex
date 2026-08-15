package auth

import (
	"testing"
	"time"
)

func TestTOTPGenerationAndValidation(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("unexpected error generating secret: %v", err)
	}
	if len(secret) == 0 {
		t.Fatal("expected non-empty secret")
	}

	code, err := GenerateTOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("unexpected error generating code: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("expected 6-digit code, got %s", code)
	}

	// Should validate current code
	if !ValidateTOTPCode(secret, code) {
		t.Fatalf("expected code %s to be valid for secret %s", code, secret)
	}

	// Should fail invalid code
	if ValidateTOTPCode(secret, "000000") && code != "000000" {
		t.Fatal("expected invalid code to fail validation")
	}

	// Should fail malformed code
	if ValidateTOTPCode(secret, "123") {
		t.Fatal("expected short code to fail validation")
	}
}

func TestTOTPClockDrift(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("unexpected error generating secret: %v", err)
	}

	// Code from 25 seconds ago (within 30s window)
	pastCode, err := GenerateTOTPCode(secret, time.Now().Add(-25*time.Second))
	if err != nil {
		t.Fatalf("generate past code: %v", err)
	}
	if !ValidateTOTPCode(secret, pastCode) {
		t.Fatalf("expected past code %s within window to be valid", pastCode)
	}

	// Code from 2 minutes ago (outside window)
	farPastCode, err := GenerateTOTPCode(secret, time.Now().Add(-120*time.Second))
	if err != nil {
		t.Fatalf("generate far past code: %v", err)
	}
	if ValidateTOTPCode(secret, farPastCode) {
		t.Fatalf("expected far past code %s to be invalid", farPastCode)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("unexpected error generating recovery codes: %v", err)
	}
	if len(codes) != 8 {
		t.Fatalf("expected 8 recovery codes, got %d", len(codes))
	}
	for _, code := range codes {
		if len(code) != 9 || code[4] != '-' { // XXXX-XXXX
			t.Fatalf("unexpected recovery code format: %s", code)
		}
	}

	normalized := NormalizeRecoveryCode("abcd-efgh")
	if normalized != "ABCD-EFGH" {
		t.Fatalf("expected ABCD-EFGH, got %s", normalized)
	}

	normalizedWithoutDash := NormalizeRecoveryCode("abcdefgh")
	if normalizedWithoutDash != "ABCD-EFGH" {
		t.Fatalf("expected ABCD-EFGH, got %s", normalizedWithoutDash)
	}
}

func TestFormatTOTPURL(t *testing.T) {
	url := FormatTOTPURL("testuser", "JBSWY3DPEHPK3PXP")
	expectedPrefix := "otpauth://totp/Pionex%20Control:testuser?secret=JBSWY3DPEHPK3PXP"
	if len(url) == 0 || url[:len(expectedPrefix)] != expectedPrefix {
		t.Fatalf("expected URL starting with %s, got %s", expectedPrefix, url)
	}
}
