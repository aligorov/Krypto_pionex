package autogrid

import "github.com/shopspring/decimal"

// NEUTRAL take-profit harvest doctrine (v2.0.160).
//
// The pre-160 DYNAMIC target fed the exchange card a profit_amount derived
// from notional × (distance entry → top of range) − friction. A neutral grid
// never monetizes that distance — its inventory only oscillates inside the
// range — so prod cards carried $15–44 targets on $50–100 slots
// (2026-09-29: DOGE $15.59, MSTRX $21.94) that structurally never fired:
// every bot exited through the stop-radar instead, and the day's realized
// payoff collapsed to a coin flip (11 wins +$6.29 vs 8 losses −$5.02,
// median TP +$0.04).
//
// The harvest amount is 2% of the bot's COMMITTED investment (floor $1):
// the same day's real harvests (SEI +$1.70, ETC +$1.42, RENDER +$1.16,
// SOXSX +$1.14 on $50–100 slots) all landed inside 1.1–3.4% of investment
// within 8–15h. The doctrine only ever LOWERS a computed target — a quieter
// pair keeps its smaller dynamic amount — and never touches FIXED mode (the
// operator's explicit number). Tranche-aware by construction: the cap reads
// the committed (post-tranche-halving) investment, and the tranche-2 top-up
// doubles investment and target together, preserving the 2% ratio.
const (
	neutralHarvestTPPct   = 2.0
	neutralHarvestTPFloor = 1.0
)

// NeutralHarvestTPCap returns the ceiling take-profit (USDT profit_amount)
// for a neutral grid bot committed to the given investment.
func NeutralHarvestTPCap(investment decimal.Decimal) decimal.Decimal {
	cap := investment.
		Mul(decimal.NewFromFloat(neutralHarvestTPPct)).
		Div(decimal.NewFromInt(100)).
		Round(2)
	if floor := decimal.NewFromFloat(neutralHarvestTPFloor); cap.LessThan(floor) {
		return floor
	}
	return cap
}

// applyNeutralHarvestTP lowers target to the harvest ceiling. FIXED mode and
// nil/zero targets pass through untouched.
func applyNeutralHarvestTP(mode string, investment decimal.Decimal, target *decimal.Decimal) *decimal.Decimal {
	if target == nil || !target.GreaterThan(decimal.Zero) || mode == "FIXED" {
		return target
	}
	cap := NeutralHarvestTPCap(investment)
	if target.GreaterThan(cap) {
		return &cap
	}
	return target
}
