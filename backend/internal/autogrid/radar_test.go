package autogrid

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func TestEvaluateConfluenceLongPullback(t *testing.T) {
	// Candidate at 20% of channel (pullback zone) with healthy ADX and Choppiness
	cand := Candidate{
		Symbol:           "TEST_USDT_PERP",
		CurrentPrice:     decimal.NewFromFloat(0.044),
		LowerPrice:       decimal.NewFromFloat(0.040),
		UpperPrice:       decimal.NewFromFloat(0.060),
		RecommendedTrend: "long",
		ModelAssumptions: map[string]any{
			"rangePositionPct": 20.0,
			"adx":              18.0,
			"choppiness":       55.0,
		},
	}

	// 15m candle with long lower wick (Hammer)
	candles := []pionex.KlineCandle{
		{
			Open:  decimal.NewFromFloat(0.046),
			High:  decimal.NewFromFloat(0.0465),
			Low:   decimal.NewFromFloat(0.042),
			Close: decimal.NewFromFloat(0.045), // Lower wick = 0.045 - 0.042 = 0.003 (66% of 0.0045 span)
		},
	}

	eval := EvaluateConfluence(cand, candles, nil)
	if eval.Score < 75.0 {
		t.Errorf("expected confluence score >= 75.0 for ideal hammer pullback, got %.2f", eval.Score)
	}
	if !eval.IsFired {
		t.Errorf("expected IsFired = true, got false")
	}
	if eval.Status != "TRIGGERED" {
		t.Errorf("expected status TRIGGERED, got %s", eval.Status)
	}
}

func TestEvaluateConfluenceAntiFOMORejection(t *testing.T) {
	// Long candidate at 85% of channel (pumping on top)
	cand := Candidate{
		Symbol:           "PUMP_TOP_USDT_PERP",
		CurrentPrice:     decimal.NewFromFloat(0.058),
		LowerPrice:       decimal.NewFromFloat(0.040),
		UpperPrice:       decimal.NewFromFloat(0.060),
		RecommendedTrend: "long",
		ModelAssumptions: map[string]any{
			"rangePositionPct": 90.0,
			"adx":              45.0,
			"choppiness":       25.0,
		},
	}

	eval := EvaluateConfluence(cand, nil, nil)
	if eval.IsFired {
		t.Errorf("expected Anti-FOMO to block entry on top, but IsFired was true")
	}
	if eval.Status != "OVERHEATED" {
		t.Errorf("expected status OVERHEATED, got %s", eval.Status)
	}
	if len(eval.RejectionReasons) == 0 {
		t.Errorf("expected rejection reasons to be present")
	}
}
