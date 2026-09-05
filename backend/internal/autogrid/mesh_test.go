package autogrid

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

func TestComputeAdaptiveMeshDensityFromMargin(t *testing.T) {
	// A true 4% span around the price: (102−98)/100.
	lower := decimal.NewFromFloat(98)
	upper := decimal.NewFromFloat(102)
	price := decimal.NewFromFloat(100)
	budget := decimal.NewFromFloat(100)

	// v2.0.75 margin-density doctrine: $100×2x = $200 notional on a 4% span
	// → 14 levels × $14.30 (step ≈0.286%). v2.0.90: the density floor is
	// harmonized with the fee-gate (2× round-trip at 5/2 bps = 0.28%) —
	// the old 0.25% floor produced geometry the fee-gate rejected by
	// 0.03% and the fleet starved. 16→14 levels is the honest dense shape.
	res := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 5, 2)
	if res.GridNum != 14 {
		t.Errorf("expected 14 levels for $200 notional on a 4%% span, got %d", res.GridNum)
	}
	step, _ := res.GridStepPct.Float64()
	if step < 0.27 || step > 0.30 {
		t.Errorf("expected ~0.286%% step, got %.4f%%", step)
	}
	if perLevel := 200.0 / float64(res.GridNum); perLevel < marketdata.MinGridLevelNotionalUSDT {
		t.Errorf("every level must carry ≥ $8, got %.2f", perLevel)
	}

	// Thin notional widens the step to keep $8/level: $25×2x = $50 notional
	// on 4% → 6 levels × $8.33 (the operator's clamp-down case).
	thin := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", decimal.NewFromFloat(25), 2, 5, 2)
	if thin.GridNum != 6 {
		t.Errorf("expected 6 levels for $50 notional on a 4%% span, got %d", thin.GridNum)
	}
	// A high-ATR regime no longer sparses the grid — density follows margin.
	wild := ComputeAdaptiveMesh(lower, upper, price, 5.0, "VOLATILE", budget, 2, 5, 2)
	if wild.GridNum != res.GridNum {
		t.Errorf("regime/ATR must not change density anymore: %d vs %d", wild.GridNum, res.GridNum)
	}
	// Degenerate geometry keeps the 8-level fallback.
	degenerate := ComputeAdaptiveMesh(upper, lower, price, 0.5, "RANGE", budget, 2, 5, 2)
	if degenerate.GridNum != 8 {
		t.Errorf("degenerate geometry must fall back to 8 levels, got %d", degenerate.GridNum)
	}
}

// v2.0.93 FIX-A: the density floor follows the caller's fee/slippage pair —
// the same numbers the deploy-time fee-gate reads. At 20/10 bps the fee-gate
// bar is 1.20% (2× the 0.60% round trip), so a 12% span that carries 25
// levels at the 5/2 fleet default collapses to 10; the operator raising fees
// shrinks the fleet's density ceiling in lockstep instead of starving it at
// a gate the floor no longer matches.
func TestComputeAdaptiveMeshFloorFollowsFees(t *testing.T) {
	lower := decimal.NewFromFloat(94)
	upper := decimal.NewFromFloat(106)
	price := decimal.NewFromFloat(100)
	budget := decimal.NewFromFloat(100)

	defaults := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 5, 2)
	if defaults.GridNum != 25 {
		// $200 notional on a 12% span: the $8-per-level cap binds
		// (8×12/200 = 0.48% step) → floor(12/0.48) = 25 levels.
		t.Fatalf("5/2 bps on a 12%% span at $200 notional = 25 levels, got %d", defaults.GridNum)
	}
	pricy := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 20, 10)
	if pricy.GridNum != 10 {
		t.Fatalf("20/10 bps bar 1.20%% → 10 levels on a 12%% span, got %d", pricy.GridNum)
	}
	step, _ := pricy.GridStepPct.Float64()
	if step < 1.20 {
		t.Fatalf("pricy-fee step must clear the 1.20%% fee-gate bar, got %.4f%%", step)
	}
	if pricy.GridNum >= defaults.GridNum {
		t.Fatalf("pricier fees must thin the grid: %d vs %d", pricy.GridNum, defaults.GridNum)
	}
	// Unknown costs (≤0) degrade to the documented fleet-default floor.
	unknown := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 0, 0)
	if unknown.GridNum != defaults.GridNum {
		t.Fatalf("unknown fees must fall back to the default floor (%d levels), got %d",
			defaults.GridNum, unknown.GridNum)
	}
}

// v2.0.93 FIX-I: the ±2% boundary clamp — one helper shared by the REAL
// deploy, the paper deploy and both DGT re-center arms. A degenerate (near
// zero) ATR parks the raw stop inside the range; the clamp pushes it back
// outside the bounds the grid actually trades.
func TestClampAntiHuntStopIntoBounds(t *testing.T) {
	lower := decimal.NewFromFloat(98)
	upper := decimal.NewFromFloat(102)

	// Degenerate LONG stop INSIDE the range → clamped to lower×0.98.
	inside := decimal.NewFromFloat(99)
	got := ClampAntiHuntStopIntoBounds("LONG", lower, upper, inside)
	if want := decimal.NewFromFloat(96.04); !got.Equal(want) {
		t.Fatalf("LONG stop inside the range must clamp to lower×0.98 = %s, got %s", want, got)
	}
	got = ClampAntiHuntStopIntoBounds("NEUTRAL", lower, upper, inside)
	if want := decimal.NewFromFloat(96.04); !got.Equal(want) {
		t.Fatalf("NEUTRAL stop inside the range must clamp like LONG, want %s got %s", want, got)
	}
	// Degenerate SHORT stop inside the range → clamped to upper×1.02.
	got = ClampAntiHuntStopIntoBounds("SHORT", lower, upper, decimal.NewFromFloat(101))
	if want := decimal.NewFromFloat(104.04); !got.Equal(want) {
		t.Fatalf("SHORT stop inside the range must clamp to upper×1.02 = %s, got %s", want, got)
	}
	// A healthy stop OUTSIDE the bounds passes through untouched on both
	// sides — the clamp must not move well-formed stops.
	healthyLong := decimal.NewFromFloat(95)
	if got = ClampAntiHuntStopIntoBounds("LONG", lower, upper, healthyLong); !got.Equal(healthyLong) {
		t.Fatalf("healthy LONG stop must pass through, got %s", got)
	}
	healthyShort := decimal.NewFromFloat(105)
	if got = ClampAntiHuntStopIntoBounds("SHORT", lower, upper, healthyShort); !got.Equal(healthyShort) {
		t.Fatalf("healthy SHORT stop must pass through, got %s", got)
	}
}

// v2.0.93 FIX-K: the AI Kit adoption clamp folds the exchange's advisory
// count into the margin-density doctrine — ceiling = the doctrine count for
// the span (fee-gate floor at the actual fees + the $8/level notional cap),
// floor = the doctrine's 6 levels.
func TestClampAIGridCount(t *testing.T) {
	// A 4% span at $200 notional, 5/2 bps: doctrine count = 14. A spot AI
	// count of 150 clamps down to 14 (born viable instead of born rejected).
	if got := clampAIGridCount(4, 200, 5, 2, 150); got != 14 {
		t.Fatalf("spot AI 150 over a 4%% span must clamp to the doctrine ceiling 14, got %d", got)
	}
	// Pricier fees shrink the ceiling in lockstep (FIX-A parity): 20/10 bps
	// → floor 1.20% → the 4% span clamps to the 6-level grid floor.
	if got := clampAIGridCount(4, 200, 20, 10, 150); got != 6 {
		t.Fatalf("pricy fees must collapse the ceiling to the 6-level floor, got %d", got)
	}
	// FIX-K: a tiny AI count (spot grid of 2) adopts UP to the doctrine
	// floor of 6 — the old clamp accepted 2.
	if got := clampAIGridCount(4, 200, 5, 2, 2); got != 6 {
		t.Fatalf("a 2-level AI adoption must lift to the doctrine floor 6, got %d", got)
	}
	// $8/level notional cap binds for cheap fees: 12% span, $200 notional,
	// 2/1 bps → doctrine count 25 regardless of the AI's 400-level spot grid.
	if got := clampAIGridCount(12, 200, 2, 1, 400); got != 25 {
		t.Fatalf("the $8/level notional cap must bound adoption at 25, got %d", got)
	}
	// A count inside the window is adopted verbatim.
	if got := clampAIGridCount(12, 200, 2, 1, 20); got != 20 {
		t.Fatalf("an in-window AI count must be adopted verbatim, got %d", got)
	}
}

func TestComputeDynamicLeverage(t *testing.T) {
	// Normal volatility (ATR 1.5%) -> full base leverage 4x
	resNormal := ComputeDynamicLeverage(1.5, 4, 8.0)
	if resNormal.Leverage != 4 || resNormal.IsScaleDown {
		t.Errorf("expected leverage 4 with no scale down, got %d (scale down: %v)", resNormal.Leverage, resNormal.IsScaleDown)
	}

	// Elevated volatility (ATR 5.5%) -> 3x
	resElevated := ComputeDynamicLeverage(5.5, 4, 8.0)
	if resElevated.Leverage != 3 || !resElevated.IsScaleDown {
		t.Errorf("expected leverage 3 with scale down, got %d (scale down: %v)", resElevated.Leverage, resElevated.IsScaleDown)
	}

	// Extreme volatility (ATR 12.0%) -> 2x
	resExtreme := ComputeDynamicLeverage(12.0, 4, 8.0)
	if resExtreme.Leverage != 2 || !resExtreme.IsScaleDown {
		t.Errorf("expected leverage 2 with scale down, got %d (scale down: %v)", resExtreme.Leverage, resExtreme.IsScaleDown)
	}
}

func TestComputeAntiHuntStop(t *testing.T) {
	lower := decimal.NewFromFloat(10.0)
	upper := decimal.NewFromFloat(12.0)
	price := decimal.NewFromFloat(10.5)
	atr := decimal.NewFromFloat(0.20) // 2% ATR

	stopLong := ComputeAntiHuntStop("LONG", lower, upper, price, atr, 1.5)
	// Stop should be lower - (1.5 * 0.20) = 10.0 - 0.30 = 9.70
	expected := decimal.NewFromFloat(9.70)
	if !stopLong.Equal(expected) {
		t.Errorf("expected anti hunt stop %s, got %s", expected, stopLong)
	}

	stopShort := ComputeAntiHuntStop("SHORT", lower, upper, price, atr, 1.5)
	// Stop should be upper + 0.30 = 12.30
	expectedShort := decimal.NewFromFloat(12.30)
	if !stopShort.Equal(expectedShort) {
		t.Errorf("expected anti hunt stop short %s, got %s", expectedShort, stopShort)
	}
}

// v2.0.52 narrow-span de-gear: spans <7% cap leverage at 2x — the audit
// showed a 4x maxLoss stop 2% from entry inside one daily sigma.
func TestComputeDynamicLeverageNarrowSpan(t *testing.T) {
	res := ComputeDynamicLeverage(1.0, 4, 2.8) // LINK-class span
	if res.Leverage != 2 || !res.IsScaleDown {
		t.Fatalf("2.8%% span at base 4x must de-gear to 2x, got %dx (%s)", res.Leverage, res.Reason)
	}
	res = ComputeDynamicLeverage(1.0, 2, 3.5)
	if res.Leverage != 2 || res.IsScaleDown {
		t.Fatalf("base 2x stays 2x without a scale-down flag, got %dx", res.Leverage)
	}
	res = ComputeDynamicLeverage(1.0, 4, 6.9)
	if res.Leverage != 2 {
		t.Fatalf("6.9%% span must de-gear to 2x, got %dx", res.Leverage)
	}
	res = ComputeDynamicLeverage(1.0, 4, 7.0)
	if res.Leverage != 4 {
		t.Fatalf("7.0%% span boundary keeps base leverage, got %dx", res.Leverage)
	}
	res = ComputeDynamicLeverage(1.0, 4, 0) // span unknown
	if res.Leverage != 4 {
		t.Fatalf("unknown span (0) must not de-gear, got %dx", res.Leverage)
	}
	// ATR de-gear still stacks correctly on wide spans.
	res = ComputeDynamicLeverage(12.0, 4, 18.0)
	if res.Leverage != 2 {
		t.Fatalf("extreme ATR on wide span must scale to 2x, got %dx", res.Leverage)
	}
}
