package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

func usdt(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

func TestNeutralHarvestTPCap(t *testing.T) {
	cases := []struct {
		invest float64
		want   float64
	}{
		{50, 1.00},   // 2% of $50 tranche-1 slot
		{80, 1.60},   // LYN-class scaler slot
		{100, 2.00},  // full $100 slot
		{200, 4.00},  // doubled tranche-2 commitment
		{20, 1.00},   // below the 2% floor: $0.40 → floor $1
		{0, 1.00},    // degenerate: floor holds
	}
	for _, c := range cases {
		got := NeutralHarvestTPCap(usdt(c.invest))
		if !got.Equal(usdt(c.want)) {
			t.Errorf("NeutralHarvestTPCap(%v) = %s, want %v", c.invest, got, c.want)
		}
	}
}

func TestApplyNeutralHarvestTP(t *testing.T) {
	// Fantasy dynamic target on a $100 slot is capped to the 2% harvest amount.
	target := usdt(43.88) // prod 2026-09-29: MSTRX #1442
	got := applyNeutralHarvestTP("DYNAMIC", usdt(100), &target)
	if got == nil || !got.Equal(usdt(2)) {
		t.Fatalf("fantasy target not capped: got %v", got)
	}

	// A quieter pair's smaller dynamic target is respected (only lowered).
	small := usdt(1.5)
	got = applyNeutralHarvestTP("DYNAMIC", usdt(100), &small)
	if got == nil || !got.Equal(usdt(1.5)) {
		t.Fatalf("small target must pass through: got %v", got)
	}

	// FIXED mode is the operator's explicit amount — untouched even above cap.
	fixed := usdt(5)
	got = applyNeutralHarvestTP("FIXED", usdt(100), &fixed)
	if got == nil || !got.Equal(usdt(5)) {
		t.Fatalf("FIXED target must not be touched: got %v", got)
	}

	// nil / zero pass through.
	if got := applyNeutralHarvestTP("DYNAMIC", usdt(100), nil); got != nil {
		t.Fatalf("nil target must stay nil, got %v", got)
	}
	zero := decimal.Zero
	if got := applyNeutralHarvestTP("DYNAMIC", usdt(100), &zero); got == nil || !got.IsZero() {
		t.Fatalf("zero target must stay zero, got %v", got)
	}

	// Tranche invariance: halve the investment, halve the cap — the tranche-2
	// top-up (both doubled) preserves the 2% ratio.
	full := usdt(43.88)
	halved := applyNeutralHarvestTP("DYNAMIC", usdt(50), &full)
	if halved == nil || !halved.Equal(usdt(1)) {
		t.Fatalf("tranche-1 cap wrong: got %v", halved)
	}
	doubled := halved.Mul(usdt(2))
	if !doubled.Equal(NeutralHarvestTPCap(usdt(100))) {
		t.Fatalf("tranche-2 doubling broke the 2%% ratio: %s vs %s", doubled, NeutralHarvestTPCap(usdt(100)))
	}
}
