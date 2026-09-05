package marketdata

import (
	"strings"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// v2.0.89-A P1 fee-gate: the invariant «level step ≥ 2× round-trip costs».
// Round-trip = 2 legs × (fee + slippage); at the fleet defaults 5/2 bps the
// round trip costs 0.14% and the minimum viable step is 2 × 0.14 = 0.28%.
func TestFeeGateRoundTripMath(t *testing.T) {
	if rt := RoundTripCostPct(5, 2); rt < 0.139999 || rt > 0.140001 {
		t.Fatalf("round-trip cost at 5/2 bps must be 0.14%%, got %.4f", rt)
	}
	if rt := RoundTripCostPct(0, 0); rt != 0 {
		t.Fatalf("zero-fee round trip must be 0, got %v", rt)
	}
}

// The load-bearing numbers from the research: a 4% span over 20 levels steps
// 0.20% < 0.28% → REJECT; the same span over 12 levels steps 0.33% ≥ 0.28% →
// PASS. The boundary itself (step exactly 2× round trip) passes.
func TestFeeGateSpanOverLevels(t *testing.T) {
	rejectStep := GridStepPctForSpan(4.0, 20)
	if rejectStep < 0.1999 || rejectStep > 0.2001 {
		t.Fatalf("4%%/20 levels must step 0.20%%, got %.4f", rejectStep)
	}
	reason, violated := FeeGateRejection(rejectStep, 5, 2)
	if !violated {
		t.Fatal("4% span over 20 levels (0.20% step) must be rejected by the fee-gate")
	}
	for _, fragment := range []string{"0.20%", "0.14%", "fee-gate"} {
		if !strings.Contains(reason, fragment) {
			t.Fatalf("rejection reason must name %q, got %q", fragment, reason)
		}
	}

	passStep := GridStepPctForSpan(4.0, 12)
	if passStep < 0.3332 || passStep > 0.3334 {
		t.Fatalf("4%%/12 levels must step 0.33%%, got %.4f", passStep)
	}
	if _, violated := FeeGateRejection(passStep, 5, 2); violated {
		t.Fatal("4% span over 12 levels (0.33% step) must clear the fee-gate")
	}
	// Legacy validator follows the same doctrine now (was 1.5× friction).
	if ValidateMinGridStep(0.20, 5, 2) {
		t.Fatal("ValidateMinGridStep must refuse a 0.20% step at 5/2 bps")
	}
	if !ValidateMinGridStep(0.28, 5, 2) || !ValidateMinGridStep(0.3333, 5, 2) {
		t.Fatal("ValidateMinGridStep must accept 0.28% and 0.33% steps at 5/2 bps")
	}
}

func feeGateScanConfig() ScanConfig {
	return ScanConfig{
		Interval:            "15M",
		LookbackCandles:     80,
		MinVolume24h:        decimal.NewFromInt(1_000_000),
		MinVolatilityPct:    0.5,
		MaxVolatilityPct:    30.0,
		MinExpectedValuePct: 0.0,
		MinSharpe:           -99,
		MaxDrawdownPct:      100.0,
		MinProfitFactor:     0.0,
		BaseLeverage:        2,
		AdaptiveLeverage:    true,
		NotionalPerBot:      400, // prod density: 400/8 = 50 levels
	}
}

func feeGateTestSymbol() pionex.SymbolInfo {
	return pionex.SymbolInfo{
		Symbol: "TST_USDT", BaseCurrency: "TST", QuoteCurrency: "USDT",
		Type: "PERP", Status: "TRADING", Enabled: true,
	}
}

// scoreCandidate wiring: the gate must fire on the FINAL persisted geometry
// (support/resistance span over the density level count) and flip the
// decision to REJECTED with the fee-gate text.
func TestScoreCandidateFeeGateWiring(t *testing.T) {
	symbol := feeGateTestSymbol()
	ticker := pionex.TickerInfo{
		Symbol: "TST_USDT",
		Open:   decimal.NewFromInt(100),
		Close:  decimal.NewFromFloat(100.2),
		Amount: decimal.NewFromInt(5_000_000),
	}
	// Symmetric +2/−2 sawtooth ending exactly at the channel midline: no
	// Anti-FOMO extremes, pure mean reversion.
	candles := synthCandles(func(i int) float64 {
		if i%2 == 0 {
			return 102
		}
		return 100
	}, 80)
	volume := decimal.NewFromInt(5_000_000)

	// Fee reality from hell (200/100 bps → round trip 6%, min step 12%):
	// every sane geometry dies on commissions.
	strict := feeGateScanConfig()
	strict.FeeBps, strict.SlippageBps = 200, 100
	candidate, err := scoreCandidate(symbol, ticker, volume, candles, strict)
	if err != nil {
		t.Fatalf("scoreCandidate: %v", err)
	}
	if candidate.Decision != "REJECTED" {
		t.Fatalf("fee-gate violation must reject the candidate, got %s (%s)",
			candidate.Decision, candidate.RejectionReason)
	}
	if !strings.Contains(candidate.RejectionReason, "fee-gate") {
		t.Fatalf("rejection must carry the fee-gate text, got %q", candidate.RejectionReason)
	}

	// Zero friction: the same geometry passes and the fee-gate stays silent.
	frictionless := feeGateScanConfig()
	frictionless.FeeBps, frictionless.SlippageBps = 0, 0
	candidate, err = scoreCandidate(symbol, ticker, volume, candles, frictionless)
	if err != nil {
		t.Fatalf("scoreCandidate (frictionless): %v", err)
	}
	if strings.Contains(candidate.RejectionReason, "fee-gate") {
		t.Fatalf("zero-fee scan must not raise the fee-gate, got %q", candidate.RejectionReason)
	}
	if candidate.Decision != "ACCEPTED" {
		t.Fatalf("frictionless permissive scan must accept the sawtooth candidate, got %s (%s)",
			candidate.Decision, candidate.RejectionReason)
	}
}
