package marketdata

import "testing"

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
// 5/2 bps (fleet default) → 0.28%.
func TestGridLevelsForRangeScalesWithNotional(t *testing.T) {
	// $200 notional on a 4% span: step floor 0.28% binds (the $8-step is
	// 8×4/200 = 0.16%) → 14 levels × $14.30 — step floor harmonized with the
	// fee-gate (0.28%) in v2.0.90.
	if got := GridLevelsForRange(4, 200, 5, 2); got != 14 {
		t.Fatalf("span 4%% at $200 notional = 4/0.28 = 14 levels, got %d", got)
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
	// Unknown notional follows the bare step floor.
	if got := GridLevelsForRange(2, 0, 5, 2); got != 7 {
		t.Fatalf("span 2%% unknown notional = 2/0.28 = 7 levels, got %d", got)
	}
}

// v2.0.93 FIX-A: the fee-gate density floor follows the ACTUAL fee/slippage
// pair the fleet runs. The old pinned 0.28% floor meant an operator raising
// feeBps starved every density source at a gate the floor no longer matched
// — different fees must now produce different floors, and unknown fees must
// degrade to the documented default.
func TestGridLevelsForRangeFloorFollowsFees(t *testing.T) {
	// 20/10 bps: round trip 0.60%, fee-gate bar 1.20% → a 4% span cannot
	// even fit 6 viable levels (floor(4/1.2)=3, clamped up to 6 by the
	// doctrine's grid floor — the same honest born-clamped shape the 0.5%
	// span produces at default fees).
	if got := GridLevelsForRange(4, 200, 20, 10); got != 6 {
		t.Fatalf("span 4%% at 20/10 bps clamps to 6 levels, got %d", got)
	}
	// A span wide enough for the pricy floor: floor(12/1.2) = 10 levels,
	// realized step exactly 1.20% — the geometry clears the fee-gate bar
	// those same fees set (self-consistency of the parameterized floor).
	pricyLevels := GridLevelsForRange(12, 200, 20, 10)
	if pricyLevels != 10 {
		t.Fatalf("span 12%% at 20/10 bps (floor 1.20%%) = 10 levels, got %d", pricyLevels)
	}
	if step := GridStepPctForSpan(12, pricyLevels); !ValidateMinGridStep(step, 20, 10) {
		t.Fatalf("derived geometry must clear its own fee-gate, step %.4f%%", step)
	}
	// Cheap fees (2/1 bps → 0.12% floor) let the $8-per-level notional cap
	// govern: 8×4/200 = 0.16% → floor(4/0.16) = 25.
	if got := GridLevelsForRange(4, 200, 2, 1); got != 25 {
		t.Fatalf("span 4%% at 2/1 bps with $200 notional = 25 levels, got %d", got)
	}
	// Unknown fees (≤0) fall back to the documented fleet default (0.28%).
	if got := GridLevelsForRange(4, 200, 0, 0); got != 14 {
		t.Fatalf("unknown fees must keep the default floor (14 levels), got %d", got)
	}
	// The floor helper itself: same invariant FeeGateRejection enforces.
	if got := FeeGateStepFloorPct(5, 2); got < 0.279 || got > 0.281 {
		t.Fatalf("default-floor helper must yield 0.28%%, got %.4f", got)
	}
	if got := FeeGateStepFloorPct(20, 10); got < 1.199 || got > 1.201 {
		t.Fatalf("20/10 bps floor must yield 1.20%%, got %.4f", got)
	}
	if got := FeeGateStepFloorPct(0, 0); got < 0.279 || got > 0.281 {
		t.Fatalf("degenerate costs must yield the documented default 0.28%%, got %.4f", got)
	}
}
