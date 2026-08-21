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
	dynamicTargetMinPct      = 4.5
	dynamicTargetMaxPct      = 10.0
	dynamicLossMinPct        = 2.0
	dynamicLossMaxPct        = 5.0
	dynamicLossRangeFloorK   = 0.15
	minRiskRewardRatio       = 1.50
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
// modest ones — the numbers follow the market with a strictly positive Risk-Reward Ratio (>= 1.35:1).
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
	lossPct := clamp(lossFloor, dynamicLossMinPct, dynamicLossMaxPct)
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

// GridLevelsForRange derives the grid level count from the pair's own
// volatility: the step should track ~0.3×ATR so each level captures a real
// travel distance instead of noise, floored above the fee-dominant zone.
// Native AI Kit counts override this when an account is configured; the
// clamp keeps the futures contract range (2–500) inside practical bounds.
func GridLevelsForRange(rangePct, atrPct float64) int {
	if rangePct <= 0 {
		return 20
	}
	stepTarget := clamp(0.3*atrPct, 0.30, 3.0)
	if stepTarget <= 0 {
		return 20
	}
	levels := math.Round(rangePct / stepTarget)
	return int(clamp(levels, 10, 60))
}

// ValidateMinGridStep checks if the grid step % is large enough to exceed
// round-trip friction (taker/maker fee and slippage).
func ValidateMinGridStep(stepPct, feeBps, slippageBps float64) bool {
	frictionPct := 2.0 * (feeBps + slippageBps) / 100.0 // Bps to percent
	return stepPct >= frictionPct*1.5                   // Ensure at least 50% margin over friction
}
