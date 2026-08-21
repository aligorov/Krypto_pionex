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
	if extreme.TargetPct != dynamicTargetMaxPct || extreme.LossPct != dynamicLossMaxPct {
		t.Fatalf("clamps must hold: %+v", extreme)
	}
	flat := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 1000, ScannerVolatilityPct: 0.1, ScannerATRPct: 0.05, ScannerDrawdownPct: 0.2,
	})
	if flat.TargetPct != dynamicTargetMinPct || flat.LossPct != dynamicLossMinPct {
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
	if tight.LossPct != dynamicLossMinPct {
		t.Fatalf("3%% span must stay at the absolute floor, got %v", tight.LossPct)
	}
	// Zero span (manual deploys) contributes nothing — DD path unchanged.
	manual := ComputeDynamicTargets(DynamicTargetsInput{
		Budget: 100, ScannerVolatilityPct: 2, ScannerATRPct: 1.5, ScannerDrawdownPct: 2,
	})
	if manual.LossPct != dynamicLossMinPct {
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

func TestGridLevelsForRangeScalesWithATR(t *testing.T) {
	// Same range width: a wild pair (high ATR) needs fewer, wider levels.
	calm := GridLevelsForRange(10, 1)
	wild := GridLevelsForRange(10, 5)
	if !(wild < calm) {
		t.Fatalf("higher ATR must mean fewer levels: calm=%d wild=%d", calm, wild)
	}
	if calm == wild {
		t.Fatal("levels must differ across volatility regimes")
	}
	// Clamps hold for degenerate inputs.
	if got := GridLevelsForRange(0, 0); got != 8 {
		t.Fatalf("degenerate input must fall back to 8, got %d", got)
	}
	if got := GridLevelsForRange(100, 0.01); got != 14 {
		t.Fatalf("upper clamp must hold, got %d", got)
	}
	if got := GridLevelsForRange(2, 20); got != 6 {
		t.Fatalf("lower clamp must hold, got %d", got)
	}
}

