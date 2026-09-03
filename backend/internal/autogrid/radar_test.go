package autogrid

import (
	"testing"
	"time"

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

// TestRadarRecenterBudgetAllows pins the header contract "B4 may exceed the
// budget by one": B3 is capped at the normal per-bot budget, B4 keeps exactly
// ONE escape slot beyond it (allowed at count == max, blocked at count ==
// max+1), and higher bands share that same single slot.
func TestRadarRecenterBudgetAllows(t *testing.T) {
	const max = 2
	// B3: the normal budget governs.
	if !radarRecenterBudgetAllows(3, max-1, max) {
		t.Fatalf("B3 below the budget must be allowed")
	}
	if radarRecenterBudgetAllows(3, max, max) {
		t.Fatalf("B3 at the budget ceiling must be blocked")
	}
	// B4: one escape slot beyond the budget.
	if !radarRecenterBudgetAllows(4, max, max) {
		t.Fatalf("B4 at count == max must still be allowed (the single over-budget escape)")
	}
	if radarRecenterBudgetAllows(4, max+1, max) {
		t.Fatalf("B4 at count == max+1 must be blocked — the escape slot is spent")
	}
	if radarRecenterBudgetAllows(5, max+1, max) {
		t.Fatalf("higher bands share the single escape slot, not a fresh one")
	}
}

// TestRadarActionCooldownFor pins the dist-aware cadence contract: the window
// is 0.55·d²·1h (Brownian expected time-to-barrier at d ATR-σ, discounted),
// clamped to the original churn bounds. The flat 2h window answered the OP
// 2026-09-02 minute-knife (0.15σ) hours too late.
func TestRadarActionCooldownFor(t *testing.T) {
	// Far from the stop: 0.55·4h = 2.2h saturates at the 2h anti-churn cap —
	// safe distances keep the calibrated cadence.
	if got := radarActionCooldownFor(2.0); got != 2*time.Hour {
		t.Fatalf("dist 2σ must cap at 2h, got %v", got)
	}
	if got := radarActionCooldownFor(5.0); got != 2*time.Hour {
		t.Fatalf("any dist beyond ~1.9σ must cap at 2h, got %v", got)
	}
	// Minute-knife: 0.55·0.04h ≈ 79s floors at 15m — the fastest legal
	// reaction cadence (the 3-snapshot dwell still applies on top).
	if got := radarActionCooldownFor(0.2); got != 15*time.Minute {
		t.Fatalf("dist 0.2σ must floor at 15m, got %v", got)
	}
	if got := radarActionCooldownFor(0.15); got != 15*time.Minute {
		t.Fatalf("dist 0.15σ (the OP case) must floor at 15m, got %v", got)
	}
	// At/through the barrier (s1 = 1): maximum urgency, still the floor —
	// a zero window would bypass the dwell gate entirely.
	if got := radarActionCooldownFor(0); got != 15*time.Minute {
		t.Fatalf("dist 0 must floor at 15m, got %v", got)
	}
	if got := radarActionCooldownFor(-1); got != 15*time.Minute {
		t.Fatalf("negative dist must floor at 15m, got %v", got)
	}
	// Mid case: 1.5σ → 0.55·2.25h = 1.2375h (74m15s), inside the bounds.
	got := radarActionCooldownFor(1.5)
	if got < 74*time.Minute || got > 75*time.Minute {
		t.Fatalf("dist 1.5σ must be ~1.24h (0.55·2.25h), got %v", got)
	}
}
