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
	res := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 0.30)
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
	thin := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", decimal.NewFromFloat(25), 2, 0.30)
	if thin.GridNum != 6 {
		t.Errorf("expected 6 levels for $50 notional on a 4%% span, got %d", thin.GridNum)
	}
	// A high-ATR regime no longer sparses the grid — density follows margin.
	wild := ComputeAdaptiveMesh(lower, upper, price, 5.0, "VOLATILE", budget, 2, 0.30)
	if wild.GridNum != res.GridNum {
		t.Errorf("regime/ATR must not change density anymore: %d vs %d", wild.GridNum, res.GridNum)
	}
	// Degenerate geometry keeps the 8-level fallback.
	degenerate := ComputeAdaptiveMesh(upper, lower, price, 0.5, "RANGE", budget, 2, 0.30)
	if degenerate.GridNum != 8 {
		t.Errorf("degenerate geometry must fall back to 8 levels, got %d", degenerate.GridNum)
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
