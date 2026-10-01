package autogrid

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// v2.0.161 liquidation guard.
//
// Prod evidence (2026-09-26..29): the whole epoch drawdown was two
// single-bot tails (ORDI −$10.45, NEAR −$10.41) — native stops firing at
// prices the leveraged geometry could not survive. Meanwhile Pionex ships
// the liquidation estimate for free: futuresGrid/checkParams returns
// estimate_liquidation_price_up/down before any capital is committed, and
// the running bot detail feed carries liquidationPrice continuously. The
// client has parsed both since the early builds — nothing consumed them.
//
// Two layers:
//  1. DEPLOY GATE (liquidationGuardReason): the stored stop must beat the
//     exchange's liquidation estimate to the exit by at least
//     liqGuardMinPct of price — the survivor consensus from the r/Pionex
//     futures-grid cohort ("stop at least ~3% beyond the liquidation
//     price, it drifts"). A stop closer than that has no room for wicks,
//     funding drift and slippage before the exchange force-closes.
//  2. PROXIMITY GUARD (manage loop): when the RUNNING liquidation price
//     closes to within liqProximityClosePct of the market price, no stop
//     ladder can be trusted to execute first — close at market through the
//     standard stop path (arms the cooldown, deliberately).
//
// Fail-open on missing estimates mirrors the checkParams contract: an
// exchange that does not return the field does not block the deploy (the
// pre-existing min-investment path behaves the same).
const (
	// liqGuardMinPct is the minimum deploy-time distance between the stored
	// stop and the exchange liquidation estimate, in % of entry price.
	liqGuardMinPct = 3.0
	// liqProximityClosePct is the running-price distance to the live
	// liquidation price that force-closes the bot.
	liqProximityClosePct = 2.0
)

// liquidationGuardReason checks the deploy-time stop ladder against the
// exchange's liquidation estimates. trend is the exchange grid trend
// ("long"/"short"/"no_trend"); stopLow is the lower stop (neutral/long),
// stopHigh the upper stop (neutral) — a short's stopLow IS its upper stop.
// Returns "" when the geometry is safe or the estimates are unavailable.
func liquidationGuardReason(
	trend string,
	entry, liqDown, liqUp decimal.Decimal,
	stopLow decimal.Decimal,
	stopHigh *decimal.Decimal,
	leverage int,
) string {
	if !entry.GreaterThan(decimal.Zero) {
		return ""
	}
	norm := strings.ToLower(strings.TrimSpace(trend))
	minDist := decimal.NewFromFloat(liqGuardMinPct)
	hundred := decimal.NewFromInt(100)

	// Downside wall: every bot whose losing side is down (neutral, long).
	// Plausibility bounds (SEC-001 lesson): a wrong-side or absurdly far
	// estimate is exchange garbage, not a wall — the wall check stands down
	// instead of failing the symbol closed on nonsense.
	downSide := norm == "" || norm == "no_trend" || norm == "neutral" || norm == "long"
	if downSide && liqDown.GreaterThan(decimal.Zero) && stopLow.GreaterThan(decimal.Zero) &&
		liqDown.LessThan(stopLow) && liqDown.GreaterThan(entry.Mul(decimal.NewFromFloat(0.5))) {
		if dist := stopLow.Sub(liqDown).Div(entry).Mul(hundred); dist.LessThan(minDist) {
			return fmt.Sprintf(
				"ликвидационный гейт: нижний стоп лишь %.2f%% выше оценки ликвидации биржи (%s) при плече %dx — норма ≥%.0f%%, снизить плечо или сузить геометрию",
				dist.InexactFloat64(), liqDown.StringFixed(4), leverage, liqGuardMinPct)
		}
	}

	// Upside wall: a short's stop above entry, or a neutral's upper stop.
	var upStop *decimal.Decimal
	if norm == "short" && stopLow.GreaterThan(decimal.Zero) {
		s := stopLow
		upStop = &s
	} else if stopHigh != nil && stopHigh.GreaterThan(decimal.Zero) {
		upStop = stopHigh
	}
	if upStop != nil && liqUp.GreaterThan(decimal.Zero) &&
		liqUp.GreaterThan(*upStop) && liqUp.LessThan(entry.Mul(decimal.NewFromFloat(1.5))) {
		if dist := liqUp.Sub(*upStop).Div(entry).Mul(hundred); dist.LessThan(minDist) {
			return fmt.Sprintf(
				"ликвидационный гейт: верхний стоп лишь %.2f%% ниже оценки ликвидации биржи (%s) при плече %dx — норма ≥%.0f%%, снизить плечо или сузить геометрию",
				dist.InexactFloat64(), liqUp.StringFixed(4), leverage, liqGuardMinPct)
		}
	}
	return ""
}

// liquidationProximityBreached reports whether the market price is within
// liqProximityClosePct of the running liquidation price.
func liquidationProximityBreached(price, runningLiq decimal.Decimal) bool {
	if !price.GreaterThan(decimal.Zero) || !runningLiq.GreaterThan(decimal.Zero) {
		return false
	}
	dist := price.Sub(runningLiq).Abs().Div(price).Mul(decimal.NewFromInt(100))
	return dist.LessThan(decimal.NewFromFloat(liqProximityClosePct))
}

// zeroToNil maps a zero/empty decimal to a SQL NULL for the liq columns.
func zeroToNil(d decimal.Decimal) *decimal.Decimal {
	if !d.GreaterThan(decimal.Zero) {
		return nil
	}
	v := d
	return &v
}

// derefZero unwraps an optional decimal for the guard's value-typed calls.
func derefZero(d *decimal.Decimal) decimal.Decimal {
	if d == nil {
		return decimal.Zero
	}
	return *d
}

// FallbackLiquidationEstimates computes the wall prices from the standard
// isolated-margin liquidation formula when the exchange returns zeros:
//
//	LONG:  liq_down ≈ entry × (1 - 1/lev + MMR)
//	SHORT: liq_up   ≈ entry × (1 + 1/lev - MMR)
//	NEUTRAL: both sides (the bot can hold inventory either way)
//
// MMR (maintenance margin rate) is ~1% for Pionex perpetuals. The estimates
// are conservative: the real wall includes accumulated funding and PnL, so
// the calculated wall is CLOSER than the actual one — the guard rejects
// MORE, never less.
func FallbackLiquidationEstimates(direction string, entry decimal.Decimal, leverage int) (up, down decimal.Decimal) {
	if !entry.GreaterThan(decimal.Zero) || leverage < 1 {
		return decimal.Zero, decimal.Zero
	}
	lev := decimal.NewFromInt(int64(leverage))
	mmr := decimal.NewFromFloat(0.01) // 1% maintenance margin rate
	inverseLev := decimal.NewFromInt(1).Div(lev)
	down = entry.Mul(decimal.NewFromInt(1).Sub(inverseLev).Add(mmr))
	if down.LessThan(decimal.Zero) {
		down = decimal.Zero
	}
	up = entry.Mul(decimal.NewFromInt(1).Add(inverseLev).Sub(mmr))
	return up, down
}
