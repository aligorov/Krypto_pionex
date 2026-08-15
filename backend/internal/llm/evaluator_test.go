package llm

import (
	"testing"
)

func TestCleanJSONResponse(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "clean json",
			input:    `{"decision":"APPROVED"}`,
			expected: `{"decision":"APPROVED"}`,
		},
		{
			name:     "markdown json block",
			input:    "```json\n{\"decision\":\"APPROVED\"}\n```",
			expected: `{"decision":"APPROVED"}`,
		},
		{
			name:     "markdown block without lang",
			input:    "```\n{\"decision\":\"REJECTED\"}\n```",
			expected: `{"decision":"REJECTED"}`,
		},
		{
			name:     "text before and after json",
			input:    "Here is my analysis:\n{\"decision\":\"APPROVED\",\"confidence\":0.9}\nHope this helps!",
			expected: `{"decision":"APPROVED","confidence":0.9}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanJSONResponse(tt.input)
			if got != tt.expected {
				t.Errorf("CleanJSONResponse() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParseAuditDecision(t *testing.T) {
	raw := `
	{
	  "decision": "APPROVED",
	  "confidence": 0.88,
	  "regime": "MEAN_REVERSION",
	  "reasoning_summary": "Отличный боковик, низкий риск пробоя",
	  "rejection_reason": null,
	  "grid_params": {
	    "lower_price": "0.0285",
	    "upper_price": "0.0382",
	    "grid_count": 28,
	    "leverage": 3,
	    "stop_loss": "0.0271",
	    "take_profit_target_usd": "7.5"
	  }
	}`

	decision, err := ParseAuditDecision(raw)
	if err != nil {
		t.Fatalf("ParseAuditDecision failed: %v", err)
	}

	if decision.Decision != "APPROVED" {
		t.Errorf("expected APPROVED, got %s", decision.Decision)
	}
	if decision.Confidence != 0.88 {
		t.Errorf("expected confidence 0.88, got %f", decision.Confidence)
	}
	if decision.GridParams == nil {
		t.Fatal("expected GridParams to be non-nil")
	}
	if decision.GridParams.Leverage != 3 {
		t.Errorf("expected leverage 3, got %d", decision.GridParams.Leverage)
	}
	if decision.GridParams.GridCount != 28 {
		t.Errorf("expected grid count 28, got %d", decision.GridParams.GridCount)
	}
}

func TestParseAuditDecisionRejection(t *testing.T) {
	raw := `
	```json
	{
	  "decision": "REJECTED",
	  "confidence": 0.95,
	  "regime": "STRONG_TREND_DOWN",
	  "reasoning_summary": "Падающий нож с высоким объемом",
	  "rejection_reason": "EMA slope -7.5% confirms ongoing dump"
	}
	```
	`

	decision, err := ParseAuditDecision(raw)
	if err != nil {
		t.Fatalf("ParseAuditDecision failed: %v", err)
	}

	if decision.Decision != "REJECTED" {
		t.Errorf("expected REJECTED, got %s", decision.Decision)
	}
	if decision.RejectionReason == nil || *decision.RejectionReason == "" {
		t.Errorf("expected non-empty rejection reason")
	}
}
