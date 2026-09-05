package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	value, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return value
}

// TestTerminalTelemetryEstimateCloseCost pins estimate b: the final is the
// last telemetry total minus the taker+slippage close cost priced on the same
// row's inventory (0.05% + 0.05% = 10 bps).
func TestTerminalTelemetryEstimateCloseCost(t *testing.T) {
	final, closeCost := terminalTelemetryEstimate(
		dec(t, "-1.0"), dec(t, "200"), decimal.Zero, "USER_CANCEL")
	if !closeCost.Equal(dec(t, "0.2")) {
		t.Fatalf("close cost = 0.001 × 200 = 0.2, got %s", closeCost)
	}
	if !final.Equal(dec(t, "-1.2")) {
		t.Fatalf("final = −1.0 − 0.2 = −1.2, got %s", final)
	}

	// Flat position closes for free.
	final, closeCost = terminalTelemetryEstimate(
		dec(t, "1.5"), decimal.Zero, decimal.Zero, "STOPPED")
	if !closeCost.IsZero() || !final.Equal(dec(t, "1.5")) {
		t.Fatalf("flat close must cost nothing, got final=%s cost=%s", final, closeCost)
	}
}

// TestTerminalTelemetryEstimateStopFloor pins the executed-stop floor: the
// stop fired at the stop, not at the stale pre-crash mark — the final may not
// read better than −max_loss − close cost (prod APT: mark −2.27, stop −12).
func TestTerminalTelemetryEstimateStopFloor(t *testing.T) {
	for _, reason := range []string{"STOP_LOSS", "STOP_LOSS_NATIVE", "LIQUIDATION"} {
		final, _ := terminalTelemetryEstimate(
			dec(t, "-2.27"), dec(t, "276.392934"), dec(t, "12"), reason)
		floor := dec(t, "-12").Sub(dec(t, "0.276392934"))
		if !final.Equal(floor) {
			t.Fatalf("%s: mark −2.27 must floor at −12 − 0.276 = %s, got %s", reason, floor, final)
		}
	}

	// Mark already beyond the stop: the mark wins (the stop cannot refund).
	final, _ := terminalTelemetryEstimate(
		dec(t, "-13.78"), dec(t, "298.733175"), dec(t, "12"), "STOP_LOSS")
	if !final.Equal(dec(t, "-14.078733175")) {
		t.Fatalf("mark beyond the stop must stay the estimate, got %s", final)
	}

	// Mark-driven closes (range break, anti-hunt, radar) never take the floor.
	final, _ = terminalTelemetryEstimate(
		dec(t, "-5.2"), dec(t, "277.563571"), dec(t, "8"), "RADAR_AUTOCLOSE_STRICT")
	if !final.Equal(dec(t, "-5.477563571")) {
		t.Fatalf("radar close executes at the mark — no floor, got %s", final)
	}

	// No configured stop → nothing to floor against.
	final, _ = terminalTelemetryEstimate(
		dec(t, "-2.0"), dec(t, "50"), decimal.Zero, "STOP_LOSS")
	if !final.Equal(dec(t, "-2.05")) {
		t.Fatalf("zero max_loss → plain estimate, got %s", final)
	}
}

// TestPaperCloseFeeRate pins the calibrated stop-close composite at 10 bps —
// the previous settings-composite (7 bps) understated a protective close.
func TestPaperCloseFeeRate(t *testing.T) {
	if !paperCloseFeeRate().Equal(dec(t, "0.001")) {
		t.Fatalf("close composite must be taker 0.05%% + slippage 0.05%% = 0.001, got %s", paperCloseFeeRate())
	}
}

// TestPaperEntryFeeInitialInventory pins the entry-fee basis: directional
// grids open the full leveraged notional at market; a neutral grid opens the
// uniform-ladder inventory at the deploy price (≈ half notional at the range
// edge, ≈ nothing at mid).
func TestPaperEntryFeeInitialInventory(t *testing.T) {
	// LONG $100 × 4x: full notional $400 → fee 0.05% = $0.20.
	fee := paperEntryFee("LONG", dec(t, "90"), dec(t, "110"), 20,
		dec(t, "100"), 4, dec(t, "100"))
	if !fee.Equal(dec(t, "0.2")) {
		t.Fatalf("directional entry fee = 0.0005 × 400 = 0.2, got %s", fee)
	}

	// NEUTRAL deployed at the lower bound: half the ladder is inventory →
	// notional $200 → fee $0.10.
	fee = paperEntryFee("NEUTRAL", dec(t, "90"), dec(t, "110"), 20,
		dec(t, "100"), 4, dec(t, "90"))
	if !fee.Equal(dec(t, "0.1")) {
		t.Fatalf("neutral-at-edge entry fee = 0.0005 × 200 = 0.1, got %s", fee)
	}

	// NEUTRAL deployed at mid: no inventory yet — no taker entry.
	fee = paperEntryFee("NEUTRAL", dec(t, "90"), dec(t, "110"), 20,
		dec(t, "100"), 4, dec(t, "100"))
	if !fee.IsZero() {
		t.Fatalf("neutral-at-mid opens nothing at market, got %s", fee)
	}

	// No geometry → half-notional convention.
	fee = paperEntryFee("NEUTRAL", decimal.Zero, decimal.Zero, 20,
		dec(t, "100"), 4, decimal.Zero)
	if !fee.Equal(dec(t, "0.1")) {
		t.Fatalf("fallback = half notional 0.1, got %s", fee)
	}
}

// TestBufferedGridLevel pins the 0.03% fill buffer: a boundary kiss is not a
// fill, a genuine traverse still completes, and multi-level moves keep
// counting.
func TestBufferedGridLevel(t *testing.T) {
	lower, upper := dec(t, "90"), dec(t, "110")
	const gridNum = 20 // level width 1.0

	// Boundary kiss: price dips 0.02% below the 95.0 boundary (94.98) and
	// returns — raw mapping flips the level, buffered mapping does not.
	kiss := dec(t, "94.98")
	if raw := gridLevelForPrice(lower, upper, gridNum, kiss); raw != 4 {
		t.Fatalf("raw mapping must flip the level on a touch, got %d", raw)
	}
	if buffered := bufferedGridLevel(lower, upper, gridNum, kiss, 5); buffered != 5 {
		t.Fatalf("a 0.02%% kiss must NOT cross the level (buffer 0.03%%), got %d", buffered)
	}

	// Genuine traverse: 1% below the boundary clears the buffer.
	filled := dec(t, "94.9")
	if buffered := bufferedGridLevel(lower, upper, gridNum, filled, 5); buffered != 4 {
		t.Fatalf("a 1%% traverse must cross the level, got %d", buffered)
	}

	// Upward mirror: 0.02% above the 96.0 boundary is not a fill.
	if buffered := bufferedGridLevel(lower, upper, gridNum, dec(t, "96.02"), 5); buffered != 5 {
		t.Fatalf("upward kiss must NOT cross the level, got %d", buffered)
	}
	if buffered := bufferedGridLevel(lower, upper, gridNum, dec(t, "96.2"), 5); buffered != 6 {
		t.Fatalf("upward traverse must cross the level, got %d", buffered)
	}

	// Multi-level traverse demands the buffer only on the final level.
	if buffered := bufferedGridLevel(lower, upper, gridNum, dec(t, "92.5"), 5); buffered != 2 {
		t.Fatalf("multi-level down traverse → level 2, got %d", buffered)
	}

	// No movement → identity.
	if buffered := bufferedGridLevel(lower, upper, gridNum, dec(t, "95.5"), 5); buffered != 5 {
		t.Fatalf("no movement keeps the level, got %d", buffered)
	}
}

// TestExchangeStopClassDictionary pins which closes count as EXECUTED stops
// (max-loss floor applies) vs mark-driven closes (no floor).
func TestExchangeStopClassDictionary(t *testing.T) {
	for _, reason := range []string{"STOP_LOSS", "STOP_LOSS_NATIVE", "LIQUIDATION", "FORCE_LIQUIDATION", "LOSS_STOP"} {
		if !exchangeStopClass(reason) {
			t.Fatalf("%s must be an executed stop", reason)
		}
	}
	for _, reason := range []string{"", "USER_CANCEL", "ALREADY_CLOSED", "STOPPED",
		"STRUCT_INVALID_ANTI_HUNT", "RANGE_BREAK_DOWN", "RADAR_AUTOCLOSE_STRICT"} {
		if exchangeStopClass(reason) {
			t.Fatalf("%s executes at the decision mark — no floor", reason)
		}
	}
}
