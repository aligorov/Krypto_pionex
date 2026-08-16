package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestComputeAdaptiveMeshBreakEvenFloor(t *testing.T) {
	lower := decimal.NewFromFloat(0.020)
	upper := decimal.NewFromFloat(0.024)
	price := decimal.NewFromFloat(0.022)
	budget := decimal.NewFromFloat(100)

	// Test that even with low ATR, step never falls below BreakEvenFloorPct (0.30%)
	res := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 0.30)
	step, _ := res.GridStepPct.Float64()
	if step < BreakEvenFloorPct {
		t.Errorf("expected grid step >= %.2f%%, got %.4f%%", BreakEvenFloorPct, step)
	}
	if res.GridNum < 10 || res.GridNum > 60 {
		t.Errorf("expected grid num between 10 and 60, got %d", res.GridNum)
	}
}

func TestComputeDynamicLeverage(t *testing.T) {
	// Normal volatility (ATR 1.5%) -> full base leverage 4x
	resNormal := ComputeDynamicLeverage(1.5, 4)
	if resNormal.Leverage != 4 || resNormal.IsScaleDown {
		t.Errorf("expected leverage 4 with no scale down, got %d (scale down: %v)", resNormal.Leverage, resNormal.IsScaleDown)
	}

	// Elevated volatility (ATR 5.5%) -> 3x
	resElevated := ComputeDynamicLeverage(5.5, 4)
	if resElevated.Leverage != 3 || !resElevated.IsScaleDown {
		t.Errorf("expected leverage 3 with scale down, got %d (scale down: %v)", resElevated.Leverage, resElevated.IsScaleDown)
	}

	// Extreme volatility (ATR 12.0%) -> 2x
	resExtreme := ComputeDynamicLeverage(12.0, 4)
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
