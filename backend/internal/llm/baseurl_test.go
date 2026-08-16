package llm

import "testing"

// Regression test for audit SEC-005/SEC-006: a custom base URL must never
// redirect the stored API key to an attacker-controlled or internal host.
func TestValidateBaseURL(t *testing.T) {
	allowed := []struct{ provider, baseURL string }{
		{ProviderGemini, ""},
		{ProviderGemini, "https://generativelanguage.googleapis.com/v1beta"},
		{ProviderAnthropic, "https://api.anthropic.com/v1/messages"},
		{ProviderOpenRouter, "https://openrouter.ai/api/v1"},
		{ProviderCustom, "https://my-llm.example.com/v1"},
	}
	for _, item := range allowed {
		if err := ValidateBaseURL(item.provider, item.baseURL); err != nil {
			t.Errorf("expected %s %q to be allowed, got %v", item.provider, item.baseURL, err)
		}
	}

	rejected := []struct{ provider, baseURL string }{
		// Key exfiltration: official provider with attacker host.
		{ProviderGemini, "https://attacker.example"},
		{ProviderAnthropic, "https://api.anthropic.com.evil.io"},
		{ProviderOpenRouter, "http://openrouter.ai"}, // not https
		{ProviderGemini, "https://198.51.100.1"},     // arbitrary IP
		{ProviderCustom, "https://localhost:8080"},   // internal
		{ProviderCustom, "https://10.0.0.5/v1"},      // private range
		{ProviderCustom, "https://192.168.1.10/v1"},  // private range
		{ProviderGemini, "not a url at all"},         // unparseable
	}
	for _, item := range rejected {
		if err := ValidateBaseURL(item.provider, item.baseURL); err == nil {
			t.Errorf("expected %s %q to be rejected", item.provider, item.baseURL)
		}
	}
}
