package llm

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// TestAuditExtrasParamsCarriesCatalyst pins the persistence contract of the
// audit payload: the news catalyst and the structured rejection reason must
// survive into recommended_params (the audit UI renders the news veto from
// there). Before this guard they were parsed and silently dropped.
func TestAuditExtrasParamsCarriesCatalyst(t *testing.T) {
	rejection := "unlock cliff in 6 hours"
	decision := &AuditDecision{
		Decision:         "REJECTED",
		Confidence:       0.9,
		Regime:           "RANGE",
		ReasoningSummary: "summary",
		RejectionReason:  &rejection,
		NewsCatalyst: &NewsCatalyst{
			Detected: true, Type: "UNLOCK", Severity: "HIGH",
			Summary: "12% supply unlock", ETAHours: 6,
		},
		GridParams: &RecommendedGridParams{
			LowerPrice: decimal.NewFromInt(90), UpperPrice: decimal.NewFromInt(110),
			GridCount: 10, Leverage: 2,
			StopLoss:            decimal.NewFromInt(80),
			TakeProfitTargetUSD: decimal.NewFromInt(12),
		},
	}

	params := auditExtrasParams(decision)
	if params == nil {
		t.Fatal("params must not be nil for a full decision")
	}
	catalyst, ok := params["news_catalyst"].(map[string]any)
	if !ok {
		t.Fatalf("news_catalyst must be persisted, got %v", params["news_catalyst"])
	}
	if catalyst["type"] != "UNLOCK" || catalyst["severity"] != "HIGH" || catalyst["eta_hours"] != 6 {
		t.Fatalf("catalyst fields lost: %v", catalyst)
	}
	if params["rejection_reason"] != rejection {
		t.Fatalf("rejection_reason must be persisted, got %v", params["rejection_reason"])
	}
	// The payload must round-trip through JSON (it is marshalled into JSONB).
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["news_catalyst"]; !ok {
		t.Fatal("news_catalyst lost in JSON round-trip")
	}
}

// TestAuditExtrasParamsEmptyDecision verifies a bare decision (no catalyst,
// no grid params, no rejection reason) stores no payload instead of an empty
// object — keeping old audit rows shape-compatible.
func TestAuditExtrasParamsEmptyDecision(t *testing.T) {
	if params := auditExtrasParams(&AuditDecision{Decision: "APPROVED"}); params != nil {
		t.Fatalf("bare decision must yield nil params, got %v", params)
	}
}
