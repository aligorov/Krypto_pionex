package autogrid

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ar1Series generates a deterministic AR(1) path around `mean` with the
// exact slope b: p_{t+1} = p_t + b·(p_t − mean). Zero residuals — the OLS
// fit in ouHalfLifeSteps recovers b exactly.
func ar1Series(n int, mean, start, b float64) []float64 {
	prices := make([]float64, n)
	prices[0] = start
	for i := 1; i < n; i++ {
		prices[i] = prices[i-1] + b*(prices[i-1]-mean)
	}
	return prices
}

// TestOuHalfLifeStepsMatchesAR1Slope pins the estimator: HL_steps =
// -ln(2)/ln(1+b) for the fitted b. With b = 2^(-1/16)−1 the half-life is
// exactly 16 steps; small-|b| series agree with the -ln(2)/b approximation.
func TestOuHalfLifeStepsMatchesAR1Slope(t *testing.T) {
	// HL = 16 steps exactly.
	b16 := math.Pow(2, -1.0/16) - 1
	hl, ok := ouHalfLifeSteps(ar1Series(200, 1000, 1100, b16))
	if !ok {
		t.Fatal("mean-reverting series must produce a half-life")
	}
	if math.Abs(hl-16) > 1e-6 {
		t.Fatalf("b = 2^(-1/16)-1 must give HL = 16 steps, got %.6f", hl)
	}

	// Small-slope agreement with the -ln(2)/b approximation.
	bSmall := -0.01
	hl, ok = ouHalfLifeSteps(ar1Series(400, 100, 100.9, bSmall))
	if !ok {
		t.Fatal("small-slope mean-reverting series must produce a half-life")
	}
	approx := -math.Ln2 / bSmall
	if math.Abs(hl-approx)/approx > 0.01 {
		t.Fatalf("HL %.4f must agree with -ln(2)/b = %.4f within 1%%", hl, approx)
	}
}

// TestOuHalfLifeStepsRejectsNonMeanReverting: a trending (b ≥ 0) or
// oscillating/divergent (b ≤ −1) tape is not an OU process — no half-life,
// and the age policy then falls to its 48h ceiling.
func TestOuHalfLifeStepsRejectsNonMeanReverting(t *testing.T) {
	if _, ok := ouHalfLifeSteps(ar1Series(200, 1000, 1000, +0.005)); ok {
		t.Fatal("trending series (b>0) must not produce a half-life")
	}
	if _, ok := ouHalfLifeSteps(ar1Series(200, 1000, 1100, -1.5)); ok {
		t.Fatal("oscillating/divergent series (b<=-1) must not produce a half-life")
	}
	if _, ok := ouHalfLifeSteps(ar1Series(10, 1000, 1100, -0.05)); ok {
		t.Fatal("too few candles must refuse to fit")
	}
	// Degenerate input (constant series): zero variance, no fit.
	if _, ok := ouHalfLifeSteps(ar1Series(50, 1000, 1000, -0.05)); ok {
		t.Fatal("constant series must refuse to fit")
	}
}

// TestGridAgeVerdictClampAndRotation pins max_grid_age = clamp(2×HL, 4h,
// 48h) and the rotation boundary: HL 4h → max 8h (9h rotates, 5h lives);
// tiny and huge HLs clamp to the floor/ceiling; a non-mean-reverting tape
// ages at the ceiling; unreadable candles never rotate (fail-open).
func TestGridAgeVerdictClampAndRotation(t *testing.T) {
	meanReverting := ouSymbolReading{ok: true, halfLifeHours: 4.0}
	if v := gridAgeVerdictFor(meanReverting, 5*time.Hour); v.rotate || v.maxAgeHours != 8 {
		t.Fatalf("HL 4h → maxAge 8h; a 5h bot must live, got maxAge %.2f rotate=%v", v.maxAgeHours, v.rotate)
	}
	if v := gridAgeVerdictFor(meanReverting, 9*time.Hour); !v.rotate {
		t.Fatalf("a 9h bot on maxAge 8h must rotate, got maxAge %.2f", v.maxAgeHours)
	}

	floor := gridAgeVerdictFor(ouSymbolReading{ok: true, halfLifeHours: 1.0}, 3*time.Hour)
	if floor.maxAgeHours != ouMaxAgeFloorHours {
		t.Fatalf("HL 1h must clamp to the 4h floor, got %.2f", floor.maxAgeHours)
	}
	ceil := gridAgeVerdictFor(ouSymbolReading{ok: true, halfLifeHours: 100.0}, 49*time.Hour)
	if ceil.maxAgeHours != ouMaxAgeCeilHours {
		t.Fatalf("HL 100h must clamp to the 48h ceiling, got %.2f", ceil.maxAgeHours)
	}
	trend := gridAgeVerdictFor(ouSymbolReading{ok: true, halfLifeHours: 0}, 49*time.Hour)
	if !trend.rotate || trend.maxAgeHours != ouMaxAgeCeilHours {
		t.Fatalf("non-mean-reverting tape must age at the 48h ceiling, got maxAge %.2f rotate=%v", trend.maxAgeHours, trend.rotate)
	}
	if v := gridAgeVerdictFor(ouSymbolReading{ok: false}, 1000*time.Hour); v.rotate {
		t.Fatal("unreadable candles must never rotate a bot (fail-open)")
	}
}

// TestCandleIntervalHours pins the step-length conversion the HL hours
// conversion and the ATR ruler rely on.
func TestCandleIntervalHours(t *testing.T) {
	cases := map[string]float64{
		"15M": 0.25, "30M": 0.5, "60M": 1, "4H": 4, "1D": 24, "1W": 168,
		"": 0.25, "garbage": 0.25,
	}
	for interval, want := range cases {
		if got := candleIntervalHours(interval); got != want {
			t.Fatalf("candleIntervalHours(%q) = %v, want %v", interval, got, want)
		}
	}
}

// TestOuHalfLifeHoursConversion: HL 16 steps on the 15m scanner cadence is
// 4 hours — the synthetic tape the integration tests plant.
func TestOuHalfLifeHoursConversion(t *testing.T) {
	b16 := math.Pow(2, -1.0/16) - 1
	hl, ok := ouHalfLifeSteps(ar1Series(200, 1000, 1100, b16))
	if !ok {
		t.Fatal("fit must succeed")
	}
	hours := hl * candleIntervalHours("15M")
	if math.Abs(hours-4.0) > 1e-6 {
		t.Fatalf("16 steps × 15m must be 4 hours, got %.6f", hours)
	}
}

// TestDgtBreakRedeployReasonFamily pins the trigger family: both break
// directions, the NEUTRAL trend-stop arm and the exhausted-shift arms fire;
// profit takes, stops and structural invalidations do not.
func TestDgtBreakRedeployReasonFamily(t *testing.T) {
	triggers := []string{
		"RANGE_BREAK_UP", "RANGE_BREAK_DOWN", "RANGE_BREAK_UP_TREND_STOP",
		"RANGE_SHIFT_UP_NO_ADJUSTMENTS_LEFT", "RANGE_SHIFT_DOWN_NO_ADJUSTMENTS_LEFT",
	}
	for _, reason := range triggers {
		if !dgtBreakRedeployReason(reason) {
			t.Fatalf("%s must trigger the DGT re-deploy", reason)
		}
	}
	nonTriggers := []string{
		"TAKE_PROFIT", "TRAILING_TAKE_PROFIT", "BREAKEVEN_LOCK",
		"STOP_LOSS", "STRUCT_INVALID_ANTI_HUNT", "RANGE_BREAK_UP_PROFIT_TAKE",
		"GRID_AGED_HALF_LIFE", "", "RANGE_SHIFT_UP",
	}
	for _, reason := range nonTriggers {
		if dgtBreakRedeployReason(reason) {
			t.Fatalf("%s must NOT trigger the DGT re-deploy", reason)
		}
	}
}

// TestSlotCapitalPrefersTrancheBase pins the same-capital contract: the
// stored tranche base (the slot's designed amount) wins over the committed
// investment; a broken base falls back to the investment.
func TestSlotCapitalPrefersTrancheBase(t *testing.T) {
	base := "400"
	if got := slotCapital(&base, decimal.NewFromInt(200)); !got.Equal(decimal.NewFromInt(400)) {
		t.Fatalf("tranche base must win, got %s", got)
	}
	broken := "not-a-number"
	if got := slotCapital(&broken, decimal.NewFromInt(200)); !got.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("broken tranche base must fall back to the investment, got %s", got)
	}
	if got := slotCapital(nil, decimal.NewFromInt(250)); !got.Equal(decimal.NewFromInt(250)) {
		t.Fatalf("nil tranche base must fall back to the investment, got %s", got)
	}
}
