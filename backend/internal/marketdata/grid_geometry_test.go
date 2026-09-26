package marketdata

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestComputeGaussianGridLevels(t *testing.T) {
	lower := decimal.NewFromFloat(100.0)
	upper := decimal.NewFromFloat(200.0)
	center := decimal.NewFromFloat(150.0)
	gridNum := 21

	levels := ComputeGaussianGridLevels(lower, upper, center, gridNum, 2)
	if len(levels) != gridNum {
		t.Fatalf("expected %d levels, got %d", gridNum, len(levels))
	}

	// Verify boundaries
	if !levels[0].Equal(lower) {
		t.Errorf("expected level[0] == %s, got %s", lower.String(), levels[0].String())
	}
	if !levels[gridNum-1].Equal(upper) {
		t.Errorf("expected level[%d] == %s, got %s", gridNum-1, upper.String(), levels[gridNum-1].String())
	}

	// Verify strictly monotonic increasing
	for i := 1; i < gridNum; i++ {
		if !levels[i].GreaterThan(levels[i-1]) {
			t.Errorf("levels not strictly monotonic at i=%d: %s <= %s", i, levels[i].String(), levels[i-1].String())
		}
	}

	// Verify Gaussian density: step near center should be smaller than step near wings
	centerIdx := gridNum / 2
	centerStep := levels[centerIdx+1].Sub(levels[centerIdx])
	wingStep := levels[gridNum-1].Sub(levels[gridNum-2])

	if !centerStep.LessThan(wingStep) {
		t.Errorf("expected centerStep (%s) < wingStep (%s) due to Gaussian clustering", centerStep.String(), wingStep.String())
	}
}
