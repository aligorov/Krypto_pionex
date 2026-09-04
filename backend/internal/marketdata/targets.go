package marketdata

import (
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
	// gridLevelsMin/Max clamp the count: 6 keeps a grid a grid, 500 is the
	// Pionex futures contract row ceiling.
	gridLevelsMin = 6
	gridLevelsMax = 500
)

// GridLevelsForRange derives the grid level count for a span (%) under the
// margin-density doctrine: the step is max(0.25%, the step at which every
// level still carries ≥ $8 of the budget×leverage notional). notional ≤ 0
// (unknown) falls back to the pure step floor. floor() — not round() — is
// load-bearing: rounding UP past notional/$8 would silently shrink the
// per-level notional below the floor the whole formula exists to protect.
func GridLevelsForRange(rangePct, notionalUSDT float64) int {
	if rangePct <= 0 {
		return 8
	}
	stepPct := GridStepFloorPct
	if notionalUSDT > 0 {
		if minOrderStep := MinGridLevelNotionalUSDT * rangePct / notionalUSDT; minOrderStep > stepPct {
			stepPct = minOrderStep
		}
	}
	levels := math.Floor(rangePct / stepPct)
	return int(clamp(levels, gridLevelsMin, gridLevelsMax))
}

// ValidateMinGridStep checks if the grid step % is large enough to exceed
// round-trip friction (taker/maker fee and slippage).
func ValidateMinGridStep(stepPct, feeBps, slippageBps float64) bool {
	frictionPct := 2.0 * (feeBps + slippageBps) / 100.0 // Bps to percent
	return stepPct >= frictionPct*1.5                   // Ensure at least 50% margin over friction
}
