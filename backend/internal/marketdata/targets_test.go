package marketdata

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestComputeDynamicTargetsScalesWithVolatility(t *testing.T) {
	quiet := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 200, ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 5,
	})
	wild := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 200, ScannerVolatilityPct: 20, ScannerATRPct: 12, ScannerDrawdownPct: 30,
	})
	// v2.0.23: the 6% ceiling compresses extreme-vol targets instead of
	// letting them run to 15% — scaling stays monotone but bounded.
	if !(wild.TargetUSDT > quiet.TargetUSDT) {
		t.Fatalf("wild market must yield a larger target: quiet=%v wild=%v", quiet.TargetUSDT, wild.TargetUSDT)
	}
	if wild.TargetPct != dynamicTargetMaxPct {
		t.Fatalf("extreme volatility must clamp at the %.1f%% ceiling, got %v", dynamicTargetMaxPct, wild.TargetPct)
	}
	if !(quiet.TargetUSDT > 0 && wild.TargetUSDT > 0) {
		t.Fatalf("targets must be positive: %+v %+v", quiet, wild)
	}
}

func TestComputeDynamicTargetsPrefersAIKit(t *testing.T) {
	result := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, AIVolatilityPct: 8, AIDrawdownPct: 6,
		ScannerVolatilityPct: 2, ScannerATRPct: 1, ScannerDrawdownPct: 3,
	})
	if result.VolSource != "pionex_ai_kit" || result.DrawdownSource != "pionex_ai_kit" {
		t.Fatalf("AI Kit readings must win, got %+v", result)
	}
	// 0.85 × 8% = 6.8% of 100 → 6.8 USDT.
	if result.TargetUSDT < 6.79 || result.TargetUSDT > 6.81 {
		t.Fatalf("expected target 6.8 USDT, got %v", result.TargetUSDT)
	}
}

func TestComputeDynamicTargetsPositiveRRR(t *testing.T) {
	result := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 1000, AIVolatilityPct: 2, AIDrawdownPct: 8,
	})
	if result.TargetUSDT < result.MaxLossUSDT*minRiskRewardRatio {
		t.Fatalf("target must be at least 1.50x max loss: target=%f loss=%f", result.TargetUSDT, result.MaxLossUSDT)
	}
}

func TestComputeDynamicTargetsClamps(t *testing.T) {
	extreme := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 1000, ScannerVolatilityPct: 90, ScannerATRPct: 60, ScannerDrawdownPct: 95,
	})
	if extreme.TargetPct != dynamicTargetMaxPct || extreme.LossPct != DynamicLossMaxPct {
		t.Fatalf("clamps must hold: %+v", extreme)
	}
	flat := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 1000, ScannerVolatilityPct: 0.1, ScannerATRPct: 0.05, ScannerDrawdownPct: 0.2,
	})
	if flat.TargetPct != dynamicTargetMinPct || flat.LossPct != DynamicLossMinPct {
		t.Fatalf("floor clamps must hold: %+v", flat)
	}
	if flat.TargetUSDT != 45 || flat.MaxLossUSDT != 20 {
		t.Fatalf("floored USDT values wrong: %+v", flat)
	}
}

// v2.0.24: the loss floor couples to the DEPLOYED grid span — a $-stop below
// a full normal traverse fires inside the grid's own oscillation (pre-fix the
// 1.0 floor tripped on every range ≥ 8.3%).
func TestComputeDynamicTargetsRangeCoupledLossFloor(t *testing.T) {
	wide := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, Leverage: 2, RangeSpanPct: 25,
		ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 1,
	})
	if wide.LossPct < 3.74 || wide.LossPct > 3.76 {
		t.Fatalf("25%% span must floor the loss at 0.15×25 = 3.75, got %v", wide.LossPct)
	}
	if wide.TargetPct < minRiskRewardRatio*wide.LossPct-1e-9 {
		t.Fatalf("RR floor must hold: target %v loss %v", wide.TargetPct, wide.LossPct)
	}
	// A tight grid keeps the absolute floor — no over-stopping quiet pairs.
	tight := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, RangeSpanPct: 3,
		ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 1,
	})
	if tight.LossPct != DynamicLossMinPct {
		t.Fatalf("3%% span must stay at the absolute floor, got %v", tight.LossPct)
	}
	// Zero span (manual deploys) contributes nothing — DD path unchanged.
	manual := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 2,
	})
	if manual.LossPct != DynamicLossMinPct {
		t.Fatalf("zero span must fall back to the DD floor, got %v", manual.LossPct)
	}
}

// v2.0.24: the 4.0 loss ceiling repairs the silent RR break — with the
// target capped at 6, any lossPct above 6/1.35 used to clamp the ratio to
// as low as 1.0. The invariant must hold across the whole input space.
func TestComputeDynamicTargetsRRInvariant(t *testing.T) {
	for _, dd := range []float64{1, 3, 5, 8, 12, 20, 40, 95} {
		for _, vol := range []float64{1, 3, 8, 20, 60} {
			for _, span := range []float64{0, 3, 8, 15, 25} {
				got := ComputeDynamicTargets(DynamicTargetsInput{
					Budget: 100, Leverage: 2, RangeSpanPct: span,
					ScannerVolatilityPct: vol, ScannerATRPct: vol / 2, ScannerDrawdownPct: dd,
				})
				if got.TargetPct < minRiskRewardRatio*got.LossPct-1e-9 {
					t.Fatalf("RR invariant broken at dd=%v vol=%v span=%v: %+v", dd, vol, span, got)
				}
			}
		}
	}
}

func TestComputeDynamicTargetsScaleWithLeverage(t *testing.T) {
	base := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 5,
	})
	lev2 := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, Leverage: 2, ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 5,
	})
	lev4 := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, Leverage: 4, ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 5,
	})
	// The PnL model marks directional bots on budget×leverage notional; the
	// USDT amounts must scale with it or the stop distance in PRICE terms
	// collapses (prod SKHY #328: $1 stop on $200 notional = 0.5% move).
	if !(lev2.TargetUSDT == 2*base.TargetUSDT && lev2.MaxLossUSDT == 2*base.MaxLossUSDT) {
		t.Fatalf("2x leverage must double USDT amounts: base=%+v lev2=%+v", base, lev2)
	}
	if !(lev4.MaxLossUSDT == 4*base.MaxLossUSDT) {
		t.Fatalf("4x leverage must quadruple the loss: base=%f lev4=%f", base.MaxLossUSDT, lev4.MaxLossUSDT)
	}
	if !(base.TargetUSDT/lev2.TargetUSDT == base.MaxLossUSDT/lev2.MaxLossUSDT) {
		t.Fatal("RR ratio must be unchanged by leverage scaling")
	}
}

// v2.0.75 margin-density doctrine: density scales with the notional
// (budget×leverage), not with a fee-floor guess. The step is
// max(fee-gate floor, the step at which every level still carries ≥ $8).
// v2.0.93 FIX-A: the floor is derived from the caller's fee/slippage pair —
// 5/2 bps (fleet default) → 0.35% since v2.0.94 (2.5× round-trip).
func TestGridLevelsForRangeScalesWithNotional(t *testing.T) {
	// $200 notional on a 4% span: step floor 0.35% binds (the $8-step is
	// 8×4/200 = 0.16%) → 11 levels × $18.20 — step floor harmonized with the
	// fee-gate (0.35%) in v2.0.94; the mining showed the old 0.28% band
	// carried every stop-out of the week.
	if got := GridLevelsForRange(4, 200, 5, 2); got != 7 {
		t.Fatalf("span 4%% at $200 notional = 4/0.504 = 7 levels (3.6x doctrine), got %d", got)
	}
	// $50 notional on the same span: the $8 floor binds (step 0.64%) →
	// round(6.25) = 6 levels, each carrying ≥ $8.
	thin := GridLevelsForRange(4, 50, 5, 2)
	if thin != 6 {
		t.Fatalf("span 4%% at $50 notional must clamp to 6 levels, got %d", thin)
	}
	if perLevel := 50.0 / float64(thin); perLevel < 8.0 {
		t.Fatalf("every level must carry ≥ $8, got %.2f", perLevel)
	}
	// The 6-level minimum survives even with unbounded notional on a tight
	// span (a 0.5% span at $10k still clamps up from 2 to 6).
	if got := GridLevelsForRange(0.5, 10_000, 5, 2); got != 6 {
		t.Fatalf("min clamp must hold, got %d", got)
	}
	// Huge span on huge notional must not hit the old ×14 ceiling but the
	// exchange row ceiling.
	if got := GridLevelsForRange(400, 1_000_000, 5, 2); got > 500 {
		t.Fatalf("max clamp must hold at 500, got %d", got)
	}
	// Degenerate span falls back.
	if got := GridLevelsForRange(0, 200, 5, 2); got != 8 {
		t.Fatalf("degenerate span must fall back to 8, got %d", got)
	}
	// Unknown notional follows the bare step floor: floor(2/0.35) = 5 clamps
	// UP to the 6-level grid floor (born-clamped; its realized 0.33% step is
	// then honestly rejected by the deploy-time fee-gate).
	if got := GridLevelsForRange(2, 0, 5, 2); got != 6 {
		t.Fatalf("span 2%% unknown notional clamps to 6 levels, got %d", got)
	}
}

// v2.0.93 FIX-A: the fee-gate density floor follows the ACTUAL fee/slippage
// pair the fleet runs. The old pinned 0.28% floor meant an operator raising
// feeBps starved every density source at a gate the floor no longer matched
// — different fees must now produce different floors, and unknown fees must
// degrade to the documented default.
func TestGridLevelsForRangeFloorFollowsFees(t *testing.T) {
	// 20/10 bps: round trip 0.60%, fee-gate bar 1.50% (2.5× since v2.0.94) →
	// a 4% span cannot even fit 6 viable levels (floor(4/1.5)=2, clamped up
	// to 6 by the doctrine's grid floor — the same honest born-clamped shape
	// the 0.5% span produces at default fees).
	if got := GridLevelsForRange(4, 200, 20, 10); got != 6 {
		t.Fatalf("span 4%% at 20/10 bps clamps to 6 levels, got %d", got)
	}
	// A span wide enough for the pricy 3.6x floor (2.16%): floor(13/2.16) = 6
	// levels, realized step 2.17% — the geometry clears the fee-gate bar
	// those same fees set (self-consistency of the parameterized floor).
	pricyLevels := GridLevelsForRange(13, 200, 20, 10)
	if pricyLevels != 6 {
		t.Fatalf("span 13%% at 20/10 bps (floor 2.16%%) = 6 levels, got %d", pricyLevels)
	}
	if step := GridStepPctForSpan(13, pricyLevels); !ValidateMinGridStep(step, 20, 10) {
		t.Fatalf("derived geometry must clear its own fee-gate, step %.4f%%", step)
	}
	// Cheap fees (2/1 bps → 0.12% floor) let the $8-per-level notional cap
	// govern: 8×4/200 = 0.16% vs the 3.6× fee floor 0.216% → floor(4/0.216) = 18.
	if got := GridLevelsForRange(4, 200, 2, 1); got != 18 {
		t.Fatalf("span 4%% at 2/1 bps with $200 notional = 18 levels, got %d", got)
	}
	// Unknown fees (≤0) fall back to the documented fleet default (0.504%).
	if got := GridLevelsForRange(4, 200, 0, 0); got != 7 {
		t.Fatalf("unknown fees must keep the default floor (7 levels), got %d", got)
	}
	// The floor helper itself: same invariant FeeGateRejection enforces.
	if got := FeeGateStepFloorPct(5, 2); got < 0.503 || got > 0.505 {
		t.Fatalf("default-floor helper must yield 0.504%%, got %.4f", got)
	}
	if got := FeeGateStepFloorPct(20, 10); got < 2.159 || got > 2.161 {
		t.Fatalf("20/10 bps floor must yield 2.16%%, got %.4f", got)
	}
	if got := FeeGateStepFloorPct(0, 0); got < 0.503 || got > 0.505 {
		t.Fatalf("degenerate costs must yield the documented default 0.504%%, got %.4f", got)
	}
}

func TestComputeIndividualTargetPrices(t *testing.T) {
	// Test 1: LONG with Ask Wall in order book -> OFI_WALL_DEFENSE
	currPrice := decimal.NewFromFloat(100.0)
	lowerPrice := decimal.NewFromFloat(95.0)
	upperPrice := decimal.NewFromFloat(105.0)
	askWall := decimal.NewFromFloat(104.0)

	depth := &DepthProfile{
		HasAskWall:     true,
		AskWallPrice:   askWall,
		ImbalanceRatio: 0.35, // Strong imbalance
	}

	resLong := ComputeIndividualTargetPrices(AdaptiveBotTargetInput{
		Symbol:         "SOL_USDT",
		Direction:      "LONG",
		CurrentPrice:   currPrice,
		LowerPrice:     lowerPrice,
		UpperPrice:     upperPrice,
		Budget:         200.0,
		Leverage:       2,
		ATR:            2.0,
		OrderBookDepth: depth,
		MinRiskReward:  1.8,
	})

	if resLong.AdaptiveStrategy != StrategyOFIWallDefense {
		t.Fatalf("expected strategy %s, got %s", StrategyOFIWallDefense, resLong.AdaptiveStrategy)
	}
	if resLong.RiskRewardRatio < 1.8 {
		t.Fatalf("expected RR >= 1.8, got %f", resLong.RiskRewardRatio)
	}
	if !resLong.TargetPrice.GreaterThan(currPrice) {
		t.Fatalf("LONG target price %s must be above current price %s", resLong.TargetPrice, currPrice)
	}
	if !resLong.StopLossPrice.LessThan(currPrice) {
		t.Fatalf("LONG stop price %s must be below current price %s", resLong.StopLossPrice, currPrice)
	}
	// Check that target is not hardcoded to $9 or $18
	if resLong.TargetUSDT == 9.0 || resLong.TargetUSDT == 18.0 {
		t.Fatalf("target must be individualized, not rigid $9 or $18: got %f", resLong.TargetUSDT)
	}

	// Test 2: SHORT with Support shelf -> SR_MOMENTUM_RUNNER
	sr := &SRAnalysisResult{
		NearestSupport:  92.0,
		SupportStrength: 0.85,
	}
	resShort := ComputeIndividualTargetPrices(AdaptiveBotTargetInput{
		Symbol:        "ETH_USDT",
		Direction:     "SHORT",
		CurrentPrice:  currPrice,
		LowerPrice:    lowerPrice,
		UpperPrice:    upperPrice,
		Budget:        150.0,
		Leverage:      3,
		ATR:           1.5,
		SRAnalysis:    sr,
		MinRiskReward: 1.8,
	})

	if resShort.AdaptiveStrategy != StrategySRMomentumRunner {
		t.Fatalf("expected strategy %s, got %s", StrategySRMomentumRunner, resShort.AdaptiveStrategy)
	}
	if resShort.RiskRewardRatio < 1.8 {
		t.Fatalf("expected RR >= 1.8, got %f", resShort.RiskRewardRatio)
	}
	if !resShort.TargetPrice.LessThan(currPrice) {
		t.Fatalf("SHORT target price %s must be below current price %s", resShort.TargetPrice, currPrice)
	}
	if !resShort.StopLossPrice.GreaterThan(currPrice) {
		t.Fatalf("SHORT stop price %s must be above current price %s", resShort.StopLossPrice, currPrice)
	}

	// Test 3: NEUTRAL grid -> MEAN_REVERSION_OU with upper & lower stop
	resNeutral := ComputeIndividualTargetPrices(AdaptiveBotTargetInput{
		Symbol:        "BTC_USDT",
		Direction:     "NEUTRAL",
		CurrentPrice:  currPrice,
		LowerPrice:    lowerPrice,
		UpperPrice:    upperPrice,
		Budget:        300.0,
		Leverage:      1,
		ATR:           2.5,
		MinRiskReward: 1.8,
	})

	if resNeutral.AdaptiveStrategy != StrategyMeanReversionOU {
		t.Fatalf("expected strategy %s, got %s", StrategyMeanReversionOU, resNeutral.AdaptiveStrategy)
	}
	if resNeutral.StopLossHigh == nil {
		t.Fatalf("NEUTRAL grid must return StopLossHigh")
	}
	if !resNeutral.StopLossHigh.GreaterThan(upperPrice) {
		t.Fatalf("StopLossHigh %s must be above upperPrice %s", resNeutral.StopLossHigh, upperPrice)
	}
	if !resNeutral.StopLossPrice.LessThan(lowerPrice) {
		t.Fatalf("StopLossPrice %s must be below lowerPrice %s", resNeutral.StopLossPrice, lowerPrice)
	}
	// NEUTRAL target MUST NOT overshoot upper grid boundary
	if resNeutral.TargetPrice.GreaterThan(upperPrice) {
		t.Fatalf("NEUTRAL target %s must NOT overshoot upperPrice %s", resNeutral.TargetPrice, upperPrice)
	}
	// MaxLossUSDT must be clamped between [2%..5%] of notional (300 * 1 = $300 -> [$6..$15])
	if resNeutral.MaxLossUSDT < 6.0 || resNeutral.MaxLossUSDT > 15.0 {
		t.Fatalf("NEUTRAL MaxLossUSDT %f must be clamped between $6 and $15, got %f", resNeutral.MaxLossUSDT, resNeutral.MaxLossUSDT)
	}

	// Test 4: Precision rounding and wide-stop clamp [2..5%]
	resPrec := ComputeIndividualTargetPrices(AdaptiveBotTargetInput{
		Symbol:         "BTC_USDT",
		Direction:      "LONG",
		CurrentPrice:   decimal.NewFromFloat(95.94812398129841),
		LowerPrice:     decimal.NewFromFloat(80.0), // very wide stop
		UpperPrice:     decimal.NewFromFloat(110.0),
		Budget:         100.0,
		Leverage:       5, // notional = $500 -> 2..5% = [$10..$25]
		ATR:            6.0,
		MinRiskReward:  1.8,
		PricePrecision: 2,
	})

	if resPrec.TargetPrice.Exponent() < -2 {
		t.Fatalf("TargetPrice %s has exponent %d, expected <= -2 precision", resPrec.TargetPrice, resPrec.TargetPrice.Exponent())
	}
	if resPrec.StopLossPrice.Exponent() < -2 {
		t.Fatalf("StopLossPrice %s has exponent %d, expected <= -2 precision", resPrec.StopLossPrice, resPrec.StopLossPrice.Exponent())
	}
	if resPrec.MaxLossUSDT > 25.0 || resPrec.MaxLossUSDT < 10.0 {
		t.Fatalf("MaxLossUSDT %f must be clamped in [$10..$25], got %f", resPrec.MaxLossUSDT, resPrec.MaxLossUSDT)
	}

	// Test 5: "no_trend" (worker.go convention) maps to NEUTRAL & MeanReversionOU
	resNoTrend := ComputeIndividualTargetPrices(AdaptiveBotTargetInput{
		Symbol:        "DOGE_USDT",
		Direction:     "no_trend",
		CurrentPrice:  decimal.NewFromFloat(0.0934),
		LowerPrice:    decimal.NewFromFloat(0.0900),
		UpperPrice:    decimal.NewFromFloat(0.0980),
		Budget:        50.0,
		Leverage:      4,
		ATR:           0.002,
		MinRiskReward: 1.8,
	})
	if resNoTrend.AdaptiveStrategy != StrategyMeanReversionOU {
		t.Fatalf("expected strategy %s for no_trend, got %s", StrategyMeanReversionOU, resNoTrend.AdaptiveStrategy)
	}
	if resNoTrend.StopLossHigh == nil {
		t.Fatalf("no_trend (neutral) grid must return StopLossHigh")
	}
	if resNoTrend.TargetPrice.GreaterThan(decimal.NewFromFloat(0.0980)) {
		t.Fatalf("no_trend target price %s must NOT overshoot UpperPrice 0.0980", resNoTrend.TargetPrice)
	}
}

