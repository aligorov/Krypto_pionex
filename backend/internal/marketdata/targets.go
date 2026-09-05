package marketdata

import (
	"fmt"
	"math"
)

// Dynamic targets replace fixed USDT amounts: each bot's take-profit and
// stop-out are derived from what the pair's own volatility actually offers,
// scaled to the invested budget.
//
// take-profit % of budget = clamp(0.6 × effective daily volatility, 1.8..6)
// stop-out    % of budget = clamp(max(0.5 × drawdown, 0.15 × grid span), 1.0..4)
//
// The effective volatility prefers the native Pionex AI Kit reading (live
// per-pair estimate from the exchange) and falls back to the scanner's
// sigma/ATR blend; drawdown prefers the AI Kit maxDrawDown and falls back to
// the scanner model drawdown.
const (
	dynamicTargetVolFraction = 0.85
	dynamicLossDDFraction    = 0.50
	// 4.5 - 10.0%: 5% base target on $200 budget ($10.00 profit per bot cycle)
	// scaling up to 10% ($20.00) on high-momentum trends.
	dynamicTargetMinPct = 4.5
	dynamicTargetMaxPct = 10.0
	// DynamicLossMinPct is the ADAPTIVE_ATR stop-out floor (% of budget×
	// leverage notional). Exported because it is ALSO the fleet-design stop
	// floor the risk derivation (breaker, tranche-2 cap) budgets against —
	// a designed bot can never store a smaller stop than this.
	DynamicLossMinPct = 2.0
	// DynamicLossMaxPct is the stop-out ceiling of the same formula. The
	// tranche-2 per-bot cap must budget against THIS bound, not the floor:
	// σ-scaled stops legally land anywhere in [Min, Max], and a floor-based
	// cap refuses exactly the wide-stop bots the top-up exists to save
	// (prod 2026-09-04: SKYAI 6x skipped ×3 with "$21.57 > $15").
	DynamicLossMaxPct      = 5.0
	dynamicLossRangeFloorK = 0.15
	minRiskRewardRatio     = 1.50
)

type DynamicTargetsInput struct {
	Budget float64
	// Leverage scales the USDT amounts to the exposure the PnL model
	// actually measures: mark-to-market computes directional PnL on
	// budget×leverage notional, so an unscaled 1%-of-budget stop fires on a
	// ~0.5% price move at 2x and ~0.25% at 4x — pure noise (prod: SHORT
	// #328 STOP_LOSS 45 minutes after deploy). Scaling both amounts keeps
	// the stop distance in PRICE terms constant across leverage while
	// preserving the RR ratio. 0 or 1 = unscaled (1x).
	Leverage int
	// AIVolatilityPct / AIDrawdownPct come from the native Pionex AI Kit
	// when an account is configured; zero means "not available".
	AIVolatilityPct float64
	AIDrawdownPct   float64
	// ScannerVolatilityPct is the annualized-to-daily sigma estimate and
	// ScannerATRPct the ATR(14)/price reading from the regime detector.
	ScannerVolatilityPct float64
	ScannerATRPct        float64
	ScannerDrawdownPct   float64
	// RangeSpanPct is the DEPLOYED grid span (upper−lower)/mid in % — the
	// quantity the PnL model actually marks against. It couples the loss
	// floor to a full normal traverse (see dynamicLossRangeFloorK) so the
	// $-stop sits structurally outside the grid's own oscillation. 0 =
	// unknown (manual deploys) — the floor then contributes nothing.
	RangeSpanPct float64
}

type DynamicTargets struct {
	TargetUSDT     float64
	MaxLossUSDT    float64
	TargetPct      float64
	LossPct        float64
	VolSource      string
	DrawdownSource string
}

// ComputeDynamicTargets turns market readings into a per-bot PnL target and
// stop-out in USDT. Wide, wild ranges earn bigger targets; quiet ranges get
// modest ones — the numbers follow the market with a strictly positive Risk-Reward Ratio (>= 1.50:1).
func ComputeDynamicTargets(input DynamicTargetsInput) DynamicTargets {
	vol, volSource := input.AIVolatilityPct, "pionex_ai_kit"
	if vol <= 0 {
		// Blend the scanner sigma with ATR: sigma captures overall dispersion,
		// ATR the recent intraday travel a grid actually harvests.
		vol = 0.5*input.ScannerVolatilityPct + 0.5*input.ScannerATRPct
		volSource = "scanner_sigma_atr_blend"
	}
	drawdown, ddSource := input.AIDrawdownPct, "pionex_ai_kit"
	if drawdown <= 0 {
		drawdown = input.ScannerDrawdownPct
		ddSource = "scanner_model"
	}

	lossFloor := dynamicLossDDFraction * drawdown
	if input.RangeSpanPct > 0 {
		if rangeFloor := dynamicLossRangeFloorK * input.RangeSpanPct; rangeFloor > lossFloor {
			lossFloor = rangeFloor
		}
	}
	lossPct := clamp(lossFloor, DynamicLossMinPct, DynamicLossMaxPct)
	rawTargetPct := math.Max(dynamicTargetVolFraction*vol, lossPct*minRiskRewardRatio)
	targetPct := clamp(rawTargetPct, dynamicTargetMinPct, dynamicTargetMaxPct)
	budget := math.Max(input.Budget, 0)
	lev := 1.0
	if input.Leverage > 1 {
		lev = float64(input.Leverage)
	}
	return DynamicTargets{
		TargetUSDT:     budget * targetPct / 100 * lev,
		MaxLossUSDT:    budget * lossPct / 100 * lev,
		TargetPct:      targetPct,
		LossPct:        lossPct,
		VolSource:      volSource,
		DrawdownSource: ddSource,
	}
}

// Grid density doctrine (v2.0.75): DENSITY SCALES WITH THE MARGIN, not with
// a fee-floor guess. The old economics floored the step at ~0.80% and clamped
// the count at 6..14, so every $200-notional 4%-span deploy collapsed to 6
// levels of 0.64-0.75% — three such bots produced ZERO grid profit in 8h
// while the dense deployments (0.22% step, 29-60 levels) harvested
// everything. The only two economic constraints that matter:
//
//   - the step floor: 0.25% — dense enough that a normal oscillation
//     crosses levels, wide enough that maker fees (2×~2-5bps round trip)
//     stay a small fraction of the captured step;
//   - the per-level notional floor: notional/levels ≥ $8 — below it the
//     per-fill harvest is dust and the order sits near exchange minimums.
//
// levels = clamp(round(span / max(0.25%, $8-step)), 6, 500).
// $100×2x on a 4% span → 16 levels × $12.5; $50 on the same span → the
// min-order step widens to 0.64% → 6 levels × $8.33.
const (
	// GridStepFloorPct is the economic density floor for the grid step.
	GridStepFloorPct = 0.25
	// MinGridLevelNotionalUSDT is the smallest acceptable per-level order
	// notional (budget×leverage/levels).
	MinGridLevelNotionalUSDT = 8.0
	// GridLevelsMin/Max clamp the count: 6 keeps a grid a grid, 500 is the
	// Pionex futures contract row ceiling. Exported (v2.0.93): the AI Kit
	// adoption clamp and any other density source must adopt the same floor
	// the doctrine clamps to.
	GridLevelsMin = 6
	GridLevelsMax = 500
)

// GridLevelsForRange derives the grid level count for a span (%) under the
// margin-density doctrine: the step is max(feeGateFloor, the step at which
// every level still carries ≥ $8 of the budget×leverage notional), where
// feeGateFloor = 2×RoundTripCostPct at the ACTUAL fee/slippage pair the fleet
// runs (v2.0.93 parameterization: the floor used to be pinned to the
// fleet-default 5/2 bps, so an operator raising feeBps starved the fleet at a
// gate the floor no longer matched — the gate and the density source MUST
// read the same numbers). notional ≤ 0 (unknown) falls back to the pure step
// floor; fee/slippage ≤ 0 (unknown) falls back to the fleet-default floor
// (DefaultGridStepFloorPct). floor() — not round() — is load-bearing:
// rounding UP past notional/$8 would silently shrink the per-level notional
// below the floor the whole formula exists to protect.
func GridLevelsForRange(rangePct, notionalUSDT, feeBps, slippageBps float64) int {
	if rangePct <= 0 {
		return 8
	}
	stepPct := FeeGateStepFloorPct(feeBps, slippageBps)
	if notionalUSDT > 0 {
		if minOrderStep := MinGridLevelNotionalUSDT * rangePct / notionalUSDT; minOrderStep > stepPct {
			stepPct = minOrderStep
		}
	}
	levels := math.Floor(rangePct / stepPct)
	return int(clamp(levels, GridLevelsMin, GridLevelsMax))
}

// FeeGateStepFloorPct is the density step floor as a function of the ACTUAL
// round-trip costs: 2× (fee + slippage) on both legs, in percent. It is the
// same invariant ValidateMinGridStep and FeeGateRejection enforce — the
// density floor and the deploy-time fee-gate are one number, derived from one
// input. Unknown costs (fee+slippage ≤ 0) degrade to the fleet-default floor.
func FeeGateStepFloorPct(feeBps, slippageBps float64) float64 {
	if feeBps <= 0 && slippageBps <= 0 {
		return DefaultGridStepFloorPct()
	}
	return 2.0 * RoundTripCostPct(feeBps, slippageBps)
}

// DefaultGridStepFloorPct is the DOCUMENTED FALLBACK of FeeGateStepFloorPct:
// 2× the round-trip cost at the fleet-default 5/2 bps = 0.28%. It stays as
// the contract for pure paths that genuinely have no live settings (and as
// the degenerate-input floor inside FeeGateStepFloorPct); every real path
// (scanner, mesh, AI Kit clamp, manual deploy, DGT re-center) passes its own
// feeBps/slippageBps through so the density floor and the fee-gate can never
// disagree again.
func DefaultGridStepFloorPct() float64 {
	return 2.0 * RoundTripCostPct(5, 2) // 0.28% at fleet defaults
}

// RoundTripCostPct returns the friction of ONE grid level round trip in
// percent of price: two legs (buy + sell), each paying feeBps + slippageBps.
// At the fleet defaults (5 bps fee / 2 bps slippage) that is
// 2 × 7 / 100 = 0.14% — the level step must clear twice THAT.
func RoundTripCostPct(feeBps, slippageBps float64) float64 {
	return 2.0 * (feeBps + slippageBps) / 100.0 // bps → %, × 2 legs
}

// ValidateMinGridStep checks the v2.0.89 fee-gate invariant: the per-level
// step must be at least 2× the round-trip cost (fee + slippage on both
// legs). A grid whose step is below that bar pays the market more per
// traverse than it can ever harvest from it — it is guaranteed to bleed on
// commissions regardless of how often price oscillates.
func ValidateMinGridStep(stepPct, feeBps, slippageBps float64) bool {
	return stepPct >= 2.0*RoundTripCostPct(feeBps, slippageBps)
}

// FeeGateRejection is the shared fee-gate verdict (v2.0.89-A research fix,
// P1). stepPct is the FINAL realized step of the grid — span_pct / grid_num
// computed on the geometry that will actually be persisted/deployed, AFTER
// every level-count clamp (density doctrine, AI Kit row, manual row). It
// returns the operator-facing rejection reason when the 2× round-trip
// invariant is violated, or ("", false) when the step clears the bar.
func FeeGateRejection(stepPct, feeBps, slippageBps float64) (string, bool) {
	roundTripPct := RoundTripCostPct(feeBps, slippageBps)
	if stepPct >= 2.0*roundTripPct {
		return "", false
	}
	return fmt.Sprintf(
		"шаг уровня %.2f%% < 2× round-trip издержек %.2f%% — сетка гарантированно в минус на комиссиях (fee-gate)",
		stepPct, roundTripPct), true
}

// GridStepPctForSpan is the realized per-level step of a grid: the span in
// percent of midline divided by the FINAL level count. gridNum < 1 yields 0
// (callers treat 0 as "no valid geometry" and the gate falls back).
func GridStepPctForSpan(spanPct float64, gridNum int) float64 {
	if gridNum < 1 || spanPct <= 0 {
		return 0
	}
	return spanPct / float64(gridNum)
}
