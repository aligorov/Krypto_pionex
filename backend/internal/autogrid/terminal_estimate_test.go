package autogrid

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func estDec(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return parsed
}

// TestTerminalTelemetryEstimate pins the v2.0.89 estimate ladder's pure
// formula:
//
//	final = telemetry_total − (taker 0.05% + slippage 0.05%) × inventory
//
// with the stop floor for EXECUTED stops (manage STOP_LOSS and native
// STOP_LOSS_NATIVE alike): the stop fired, so the final may not read better
// than −max_loss − close_cost — the mark-to-execution gap the 2026-09-04
// cascade afternoon proved (ICP/SUI fired native stops while their last
// telemetry marks still read POSITIVE; APT's stop executed at −12 with the
// last mark at −2.27).
func TestTerminalTelemetryEstimate(t *testing.T) {
	// Plain estimate, no stop: 10 bps of the inventory notional.
	final, closeCost := terminalTelemetryEstimate(
		estDec(t, "-2.5"), estDec(t, "91.875420"), decimal.Zero, "USER_CANCEL")
	if !closeCost.Equal(estDec(t, "0.09187542")) {
		t.Fatalf("close cost = %s, want 0.09187542 (10 bps of 91.87542)", closeCost)
	}
	if !final.Equal(estDec(t, "-2.59187542")) {
		t.Fatalf("final = %s, want -2.59187542", final)
	}

	// Flat position closes for free: the total passes through untouched.
	final, closeCost = terminalTelemetryEstimate(
		estDec(t, "1.476774"), decimal.Zero, decimal.Zero, "STRUCT_INVALID_ANTI_HUNT")
	if !closeCost.IsZero() || !final.Equal(estDec(t, "1.476774")) {
		t.Fatalf("flat position must estimate at its total, got %s (cost %s)", final, closeCost)
	}

	// The prod APT shape: native stop, stale mark −2.268556 on 276.392934
	// inventory, max_loss 12 — the floor carries the execution gap.
	final, closeCost = terminalTelemetryEstimate(
		estDec(t, "-2.268556"), estDec(t, "276.392934"), estDec(t, "12"), "STOP_LOSS_NATIVE")
	if !closeCost.Equal(estDec(t, "0.276392934")) {
		t.Fatalf("APT close cost = %s, want 0.276392934", closeCost)
	}
	if !final.Equal(estDec(t, "-12.276392934")) {
		t.Fatalf("APT floored final = %s, want -12.276392934", final)
	}

	// The prod ICP shape: POSITIVE mark on a fired native stop — the floor
	// refuses the mark's fiction.
	final, _ = terminalTelemetryEstimate(
		estDec(t, "0.158457"), estDec(t, "67.184"), estDec(t, "4"), "STOP_LOSS_NATIVE")
	if !final.Equal(estDec(t, "-4.067184")) {
		t.Fatalf("ICP floored final = %s, want -4.067184", final)
	}

	// The prod SNXXX shape: manage stop whose mark is already DEEPER than the
	// stop — the deeper mark wins (the floor only bounds optimistic marks).
	final, _ = terminalTelemetryEstimate(
		estDec(t, "-13.780395"), estDec(t, "298.733175"), estDec(t, "12"), "STOP_LOSS")
	if !final.Equal(estDec(t, "-14.079128175")) {
		t.Fatalf("SNXXX final = %s, want -14.079128175 (deeper than the floor)", final)
	}

	// A stop-class reason without a max_loss witness floors nothing.
	final, _ = terminalTelemetryEstimate(
		estDec(t, "0.158457"), estDec(t, "67.184"), decimal.Zero, "STOP_LOSS_NATIVE")
	if !final.Equal(estDec(t, "0.091273")) {
		t.Fatalf("no max_loss → no floor, got %s", final)
	}

	// Non-stop loss classes never floor: a radar auto-close estimates at mark.
	final, _ = terminalTelemetryEstimate(
		estDec(t, "-5.205625"), estDec(t, "277.563571"), estDec(t, "9"), "RADAR_AUTOCLOSE_STRICT")
	if !final.Equal(estDec(t, "-5.483188571")) {
		t.Fatalf("non-stop close must estimate at mark − cost, got %s", final)
	}
}

// TestTerminalRefusalErrorClassification pins the transport-vs-refusal split
// the 30-day backfill decides on: not_found-class errors from the detail
// endpoint mean "finished grid, no exchange total will ever come" (settle
// from telemetry), while transport noise must retry next pass.
func TestTerminalRefusalErrorClassification(t *testing.T) {
	refusals := []string{
		"P_BOT_ORDER_NOT_FOUND: order not found or already closed",
		"order already_closed", "grid not_exist", "HTTP 404",
		"invalid_order in current state", "forbidden current state",
		"can not cancel in current state",
	}
	for _, message := range refusals {
		if !terminalRefusalError(errors.New(message)) {
			t.Fatalf("%q must classify as a terminal refusal", message)
		}
	}
	transports := []string{
		"connection reset by peer",
		"context deadline exceeded",
		"internal error",
	}
	for _, message := range transports {
		if terminalRefusalError(errors.New(message)) {
			t.Fatalf("%q must classify as transport (retry next pass)", message)
		}
	}
	if terminalRefusalError(nil) {
		t.Fatal("nil error is not a refusal")
	}
}
