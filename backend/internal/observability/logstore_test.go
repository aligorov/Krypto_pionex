package observability

import "testing"

func TestRedactFields(t *testing.T) {
	t.Parallel()
	input := map[string]any{
		"token": "secret",
		"safe":  "visible",
		"nested": map[string]any{
			"api_key": "secret",
			"symbol":  "BTC_USDT_PERP",
		},
	}
	output := RedactFields(input)
	if output["token"] != "[REDACTED]" {
		t.Fatal("token was not redacted")
	}
	if output["safe"] != "visible" {
		t.Fatal("safe value changed")
	}
	nested, ok := output["nested"].(map[string]any)
	if !ok || nested["api_key"] != "[REDACTED]" || nested["symbol"] != "BTC_USDT_PERP" {
		t.Fatal("nested redaction failed")
	}
}
