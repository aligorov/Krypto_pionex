package autogrid

import (
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// Stress-inventory floor (v2.0.139, package C1). The dynamic loss cap
// (DynamicLossMaxPct 5% ceiling) can sit BELOW the mark-to-market of a full
// adverse grid traverse to the anti-hunt stop: a σ-quiet pair legally derives
// a 2%-of-notional stop while its own geometry loads inventory on every level
// between entry and the lower bound — and the documented stop-out class
// (NEAR #1401, settled −$14.90 against a ~$10 cap) died exactly there. The
// ceiling exists for budget sizing; the FLOOR below is what was missing.
type stressGeometry struct {
	// direction is the deploy trend ("long"/"short"/"neutral", case-insensitive;
	// anything but SHORT accumulates on the downside).
	direction string
	entry     decimal.Decimal
	lower     decimal.Decimal
	upper     decimal.Decimal
	// stop is the deploy-time anti-hunt stop the stress traverse ends at.
	stop    decimal.Decimal
	gridNum int
	// invest is the quote capital actually committed by this deploy (already
	// tranche-halved when the deploy is a tranche-1 half).
	invest decimal.Decimal
}

// stressResult is the deploy-path view of the stress computation: the full-
// traverse loss plus whether the floor lifted the dynamic max-loss.
type stressResult struct {
	loss decimal.Decimal
	// floored is the v2.0.139 telemetry marker trigger: the dynamic cap sat
	// below the stress loss and maxLoss now carries the floor.
	floored bool
}

// stressInventoryLossUSDT simulates the inventory a grid accumulates from
// entry to the anti-hunt stop and returns its mark-to-market loss at the
// stop plus the protective-exit taker cost. Model (deterministic, cheap —
// same uniform-allocation assumption the EV model uses), calibrated against
// the codebase's own exchange-truth models:
//
//   - NEUTRAL (paperInitialInventoryNotional / neutralGridPaperPNL): a fresh
//     neutral grid opens ZERO inventory and accumulates one perLevel notional
//     per downside level; with entry at mid that bottoms out at half the
//     notional — the half-clamp neutralGridPaperPNL pins. Each downside level
//     p (entry > p ≥ stop) contributes notional×(1 − stop/p) — the COMPLETE
//     mark-to-market of that unit at the stop, including the segment below
//     the lower bound; levels below the stop never fill. NO carry leg: adding
//     a full-notional bound→stop ride on top (the first v2.0.139 draft) both
//     double-counted that segment and implied a position above margin×lev —
//     review P0-1.
//   - LONG/SHORT (v2.0.97 exchange truth, paperEntryFee): a directional grid
//     opens the FULL leveraged notional at entry — the adverse traverse is
//     notionalFull×(1 − stop/entry) (SHORT: notionalFull×(stop/entry − 1));
//     grid fills add nothing on top of an already-full position. The first
//     draft's levels+carry(bound) shape UNDERSTATED this by ~45% (P1-2) —
//     the NEAR #1401 class would have survived on trend grids.
//   - exitCost = the pionex taker+slippage composite (0.10%) on the total
//     inventory notional held at the stop.
//
// Degenerate inputs (gridNum < 1, non-positive prices/investment/leverage,
// inverted bounds, stop on the wrong side of entry for the direction)
// return zero — the caller treats that as "no floor".
func stressInventoryLossUSDT(
	direction string,
	entry, lower, upper, stop decimal.Decimal,
	gridNum int,
	investAmount decimal.Decimal,
	lev int,
) decimal.Decimal {
	if gridNum < 1 || lev < 1 || !investAmount.IsPositive() {
		return decimal.Zero
	}
	if !entry.IsPositive() || !lower.IsPositive() || !upper.IsPositive() || !stop.IsPositive() {
		return decimal.Zero
	}
	if !upper.GreaterThan(lower) {
		return decimal.Zero
	}
	dir := strings.ToUpper(strings.TrimSpace(direction))
	notionalFull := investAmount.Mul(decimal.NewFromInt(int64(lev)))
	loss := decimal.Zero
	notionalAtStop := decimal.Zero
	switch dir {
	case "LONG":
		if !stop.LessThan(entry) {
			return decimal.Zero // a long's stop sits below entry; anything else is degenerate
		}
		loss = notionalFull.Mul(decimal.NewFromInt(1).Sub(stop.Div(entry)))
		notionalAtStop = notionalFull
	case "SHORT":
		if !stop.GreaterThan(entry) {
			return decimal.Zero
		}
		loss = notionalFull.Mul(stop.Div(entry).Sub(decimal.NewFromInt(1)))
		notionalAtStop = notionalFull
	default:
		// NEUTRAL: downside level accumulation only — the level MTMs already
		// carry each unit to the stop, so there is deliberately no carry leg.
		perLevel := notionalFull.Div(decimal.NewFromInt(int64(gridNum)))
		step := upper.Sub(lower).Div(decimal.NewFromInt(int64(gridNum)))
		for i := 0; i < gridNum; i++ {
			p := lower.Add(step.Mul(decimal.NewFromInt(int64(i))))
			if !p.LessThan(entry) {
				continue // only downside levels accumulate a long inventory
			}
			if p.LessThan(stop) {
				continue // past the stop — never fills before the bot dies
			}
			loss = loss.Add(perLevel.Mul(decimal.NewFromInt(1).Sub(stop.Div(p))))
			notionalAtStop = notionalAtStop.Add(perLevel)
		}
	}
	return loss.Add(pionex.CloseCostUSDT(notionalAtStop))
}
