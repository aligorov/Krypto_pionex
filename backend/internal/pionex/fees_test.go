package pionex

import (
	"testing"

	"github.com/shopspring/decimal"
)

func feeDec(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return parsed
}

// TestEntryFeeUSDT pins the task's entry-fee formula (v2.0.89): taker 0.05%
// on the notional a deploy or pour opens — investment × leverage. The
// empirical anchor: the operator's 25-entry epoch (~$150–600 notional each)
// paid ≈$2.3 of entry fees, consistent with taker-on-notional.
func TestEntryFeeUSDT(t *testing.T) {
	cases := []struct {
		name       string
		investment string
		leverage   int
		want       string
	}{
		{"epoch canary 25x lev 6", "25", 6, "0.075"},
		{"epoch standard 100x lev 2", "100", 2, "0.1"},
		{"epoch standard 100x lev 6", "100", 6, "0.3"},
		{"tranche pour 50x lev 2", "50", 2, "0.05"},
		{"leverless", "100", 1, "0.05"},
	}
	for _, tc := range cases {
		got := EntryFeeUSDT(feeDec(t, tc.investment), tc.leverage)
		if !got.Equal(feeDec(t, tc.want)) {
			t.Fatalf("%s: EntryFeeUSDT = %s, want %s", tc.name, got, tc.want)
		}
	}
	// Degenerate inputs book nothing.
	for _, inv := range []string{"0", "-5"} {
		if !EntryFeeUSDT(feeDec(t, inv), 6).IsZero() {
			t.Fatalf("investment %s must book no fee", inv)
		}
	}
	if !EntryFeeUSDT(feeDec(t, "100"), 0).IsZero() {
		t.Fatal("leverage 0 must book no fee")
	}
}

// TestCloseCostUSDT pins the close-cost formula: taker 0.05% + slippage 0.05%
// on the inventory notional the last telemetry row carried into the close.
func TestCloseCostUSDT(t *testing.T) {
	cases := []struct {
		inventory string
		want      string
	}{
		{"1000", "1"}, // the 10 bps composite
		{"298.733175", "0.298733175"},
		{"0", "0"},  // flat position closes for free
		{"-5", "0"}, // degenerate inventory prices nothing
	}
	for _, tc := range cases {
		got := CloseCostUSDT(feeDec(t, tc.inventory))
		if !got.Equal(feeDec(t, tc.want)) {
			t.Fatalf("CloseCostUSDT(%s) = %s, want %s", tc.inventory, got, tc.want)
		}
	}
}

// TestFeeRatesAreExchangeTruth pins the rates themselves: taker 0.05%,
// slippage 0.05% — fixed exchange truth that must never drift with the
// scanner's tunable fee/slippage bps.
func TestFeeRatesAreExchangeTruth(t *testing.T) {
	if TakerFeeRate != 0.0005 {
		t.Fatalf("taker rate = %v, want 0.0005", TakerFeeRate)
	}
	if CloseCostSlippageRate != 0.0005 {
		t.Fatalf("close slippage rate = %v, want 0.0005", CloseCostSlippageRate)
	}
}
