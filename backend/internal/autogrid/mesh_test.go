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
	entry := decimal.NewFromFloat(100)
	stop := decimal.NewFromFloat(98) // 2% stop distance

	lev := ComputeDynamicLeverage(entry, stop, 1.5, 0.90, 3)
	if lev < 2 || lev > 3 {
		t.Errorf("expected leverage between 2 and 3, got %d", lev)
	}

	// High volatility asset (ATR 6.5%) should be penalized down to safe leverage
	levHighVol := ComputeDynamicLeverage(entry, stop, 6.5, 0.90, 4)
	if levHighVol > 2 {
		t.Errorf("expected high vol leverage capped at 2, got %d", levHighVol)
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
