package pionex

import (
	"net/url"
	"testing"
)

func TestSignerHMAC(t *testing.T) {
	signer := NewSigner("test_key", "test_secret")
	query := url.Values{}
	query.Set("symbol", "BTC_USDT")

	sig, err := signer.SignRequest("GET", "/api/v1/market/symbols", query, nil, 1700000000000)
	if err != nil {
		t.Fatalf("SignRequest failed: %v", err)
	}

	if sig == "" {
		t.Fatalf("Expected non-empty signature")
	}
}
