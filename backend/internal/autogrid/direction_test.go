package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestSelectDirectionTrendDown(t *testing.T) {
	// TREND_DOWN + positive funding -> SHORT (shorts earn carry)
	decision := SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0005},
		EventContext{},
	)
	if decision.Direction != "SHORT" {
		t.Errorf("expected SHORT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	if decision.Leverage != 3 {
		t.Errorf("expected leverage 3, got %d", decision.Leverage)
	}

	// v2.0.14: TREND_DOWN + modestly negative (non-extreme) funding -> SHORT
	// (dumps flip funding negative; that used to veto the best short setups)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0005},
		EventContext{},
	)
	if decision.Direction != "SHORT" {
		t.Errorf("expected SHORT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// v2.0.14: TREND_DOWN + neutral funding -> SHORT (not extreme)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "SHORT" {
		t.Errorf("expected SHORT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// v2.0.14: TREND_DOWN + EXTREME negative funding -> WAIT (crowded shorts
	// are squeeze fuel even in a downtrend)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: -0.005, IsExtreme: true},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
}

func TestSelectDirectionTrendUp(t *testing.T) {
	// TREND_UP + negative funding -> LONG (longs earn carry)
	decision := SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0005},
		EventContext{},
	)
	if decision.Direction != "LONG" {
		t.Errorf("expected LONG, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	if decision.Leverage != 3 {
		t.Errorf("expected leverage 3, got %d", decision.Leverage)
	}

	// v2.0.14: TREND_UP + modestly positive (non-extreme) funding -> LONG
	// (rallies carry positive funding; LONG used to be unreachable in every
	// up-market)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0005},
		EventContext{},
	)
	if decision.Direction != "LONG" {
		t.Errorf("expected LONG, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// v2.0.14: TREND_UP + neutral funding -> LONG (not extreme)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "LONG" {
		t.Errorf("expected LONG, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// v2.0.14: TREND_UP + EXTREME positive funding -> WAIT (crowded longs)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.005, IsExtreme: true},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
}

// v2.0.14 FNG scoping: euphoria (>=85) freezes everything; extreme panic
// (1..15) freezes directionals only — capitulation stays harvestable by
// NEUTRAL grids (Nagel: reversal premium is highest post-flush).
func TestSelectDirectionFearGreedScoping(t *testing.T) {
	panicCtx := EventContext{FearGreedExtreme: 8}
	decision := SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.7, HurstValue: 0.42},
		FundingContext{AverageRate: 0.0},
		panicCtx,
	)
	if decision.Direction != "NEUTRAL" {
		t.Errorf("panic zone must allow NEUTRAL, got %s (%s)", decision.Direction, decision.Reason)
	}
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0},
		panicCtx,
	)
	if decision.Direction != "WAIT" {
		t.Errorf("panic zone must freeze SHORT, got %s (%s)", decision.Direction, decision.Reason)
	}
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0},
		panicCtx,
	)
	if decision.Direction != "WAIT" {
		t.Errorf("panic zone must freeze LONG, got %s (%s)", decision.Direction, decision.Reason)
	}

	euphoria := EventContext{FearGreedExtreme: 88}
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.7, HurstValue: 0.42},
		FundingContext{AverageRate: 0.0},
		euphoria,
	)
	if decision.Direction != "WAIT" {
		t.Errorf("euphoria must freeze NEUTRAL too, got %s (%s)", decision.Direction, decision.Reason)
	}
}

func TestSelectDirectionRange(t *testing.T) {
	// RANGE + low Hurst -> NEUTRAL regardless of confidence: since v2.0.3 the
	// gate is the Hurst boundary (0.60, aligned with the confluence engine's
	// HurstHardVetoNeutral) and confidence is informational only — the old
	// confidence>0.55 requirement combined with a hardcoded Hurst=0.5 input
	// dead-locked every RANGE candidate into WAIT.
	decision := SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.7, HurstValue: 0.42},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "NEUTRAL" {
		t.Errorf("expected NEUTRAL, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	if decision.Leverage != 2 {
		t.Errorf("expected leverage 2 (not 4x), got %d", decision.Leverage)
	}

	// RANGE + low confidence but mean-reverting Hurst still deploys NEUTRAL
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.4, HurstValue: 0.42},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "NEUTRAL" {
		t.Errorf("expected NEUTRAL with low confidence, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// RANGE + Hurst at the neutral midpoint (0.5) — the value that used to be
	// hardcoded and made the strict `< 0.50` comparison always false
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.6, HurstValue: 0.5},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "NEUTRAL" {
		t.Errorf("expected NEUTRAL at Hurst 0.5, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// RANGE + high Hurst (trending) -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.7, HurstValue: 0.6},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// RANGE + Hurst just below the boundary (0.599) -> NEUTRAL; at 0.60 -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.7, HurstValue: 0.599},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "NEUTRAL" {
		t.Errorf("expected NEUTRAL at Hurst 0.599, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.7, HurstValue: 0.60},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT at Hurst 0.60, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
}

func TestSelectDirectionBlockingGates(t *testing.T) {
	// High impact event -> WAIT regardless of regime (even a perfect LONG setup)
	decision := SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.9, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0005},
		EventContext{HighImpactEvent24h: true},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT on high-impact event, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// Liquidation cascade (v2.0.21 semantics): a long-side unwind is the
	// SHORT participation window — TREND_DOWN shorts stay live; the shapes
	// that would load the knife (LONG/NEUTRAL) wait. The blanket freeze was
	// the 2026-08-20 audit finding: it surrendered the best short entries
	// of the cycle exactly when they set up.
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.9, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0005},
		EventContext{LiquidationCascade: true},
	)
	if decision.Direction != "SHORT" {
		t.Errorf("expected SHORT through a liquidation cascade in TREND_DOWN, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.9, HurstValue: 0.4},
		FundingContext{AverageRate: 0.0},
		EventContext{LiquidationCascade: true},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT on liquidation cascade for NEUTRAL (knife loading), got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// Both gates + fear/greed extreme still -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.9, HurstValue: 0.4},
		FundingContext{AverageRate: 0.0},
		EventContext{HighImpactEvent24h: true, LiquidationCascade: true, FearGreedExtreme: 90},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT with all gates triggered, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// Fear&Greed extremes alone block any direction (wired v2.0.8; was a
	// dead input). Euphoria >= 85 and panic 1..15 are paralysis zones; the
	// zero value 0 is no-signal and must NOT block.
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.9, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0005},
		EventContext{FearGreedExtreme: 88},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT on extreme greed, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.9, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0005},
		EventContext{FearGreedExtreme: 12},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT on extreme fear, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.9, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0005},
		EventContext{FearGreedExtreme: 0},
	)
	if decision.Direction != "LONG" {
		t.Errorf("FNG zero-value must be no-signal, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// Unknown regime -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "CHAOS", Confidence: 0.9, HurstValue: 0.4},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT on unknown regime, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
}

func TestShouldResetGridBreakout(t *testing.T) {
	lower := decimal.NewFromFloat(100)
	upper := decimal.NewFromFloat(110)
	bufferPct := decimal.NewFromFloat(1.0) // 1%

	// Price above upper + buffer (110 * 1.01 = 111.1) -> RESET_UP
	plan := ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(112), bufferPct, 3)
	if plan == nil {
		t.Fatal("expected RESET_UP plan, got nil")
	}
	if plan.Action != "RESET_UP" {
		t.Errorf("expected RESET_UP, got %s", plan.Action)
	}
	// New grid is centered on current price with the same half-width (5)
	if diff := plan.NewLower.Sub(decimal.NewFromFloat(107)); diff.Abs().GreaterThan(decimal.NewFromFloat(0.0000001)) {
		t.Errorf("expected new lower ~107, got %s", plan.NewLower)
	}
	if diff := plan.NewUpper.Sub(decimal.NewFromFloat(117)); diff.Abs().GreaterThan(decimal.NewFromFloat(0.0000001)) {
		t.Errorf("expected new upper ~117, got %s", plan.NewUpper)
	}

	// Price below lower - buffer (100 * 0.99 = 99) -> RESET_DOWN
	plan = ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(98), bufferPct, 3)
	if plan == nil {
		t.Fatal("expected RESET_DOWN plan, got nil")
	}
	if plan.Action != "RESET_DOWN" {
		t.Errorf("expected RESET_DOWN, got %s", plan.Action)
	}
	if diff := plan.NewLower.Sub(decimal.NewFromFloat(93)); diff.Abs().GreaterThan(decimal.NewFromFloat(0.0000001)) {
		t.Errorf("expected new lower ~93, got %s", plan.NewLower)
	}
	if diff := plan.NewUpper.Sub(decimal.NewFromFloat(103)); diff.Abs().GreaterThan(decimal.NewFromFloat(0.0000001)) {
		t.Errorf("expected new upper ~103, got %s", plan.NewUpper)
	}

	// Price just above upper but inside buffer (110.5 < 111.1) -> nil (hold)
	plan = ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(110.5), bufferPct, 3)
	if plan != nil {
		t.Errorf("expected nil inside buffer zone, got %s", plan.Action)
	}

	// Price just below lower but inside buffer (99.5 > 99) -> nil (hold)
	plan = ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(99.5), bufferPct, 3)
	if plan != nil {
		t.Errorf("expected nil inside buffer zone, got %s", plan.Action)
	}

	// Price in middle of range -> nil
	plan = ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(105), bufferPct, 3)
	if plan != nil {
		t.Errorf("expected nil in-range, got %s", plan.Action)
	}

	// No adjustments left -> nil even on a hard breakout
	plan = ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(120), bufferPct, 0)
	if plan != nil {
		t.Errorf("expected nil with no adjustments left, got %s", plan.Action)
	}

	// Exactly at break-up boundary (111.1) is NOT a breakout (strict >)
	plan = ShouldResetGrid("NEUTRAL", lower, upper, decimal.NewFromFloat(111.1), bufferPct, 3)
	if plan != nil {
		t.Errorf("expected nil exactly at boundary, got %s", plan.Action)
	}
}

func TestSelectDirectionCascadeShortWindow(t *testing.T) {
	regimeDown := RegimeContext{Regime: "TREND_DOWN"}
	// Active cascade overrides the FNG panic freeze for shorts — the forced
	// unwind window is the short entry, freezing it surrenders the cycle.
	got := SelectDirection(regimeDown, FundingContext{}, EventContext{
		FearGreedExtreme: 8, LiquidationCascade: true,
	})
	if got.Direction != "SHORT" {
		t.Fatalf("cascade must allow SHORT through panic, got %q (%s)", got.Direction, got.Reason)
	}
	// Without a cascade the panic freeze holds (v2.0.14 semantics).
	got = SelectDirection(regimeDown, FundingContext{}, EventContext{FearGreedExtreme: 8})
	if got.Direction != "WAIT" {
		t.Fatalf("panic without cascade must freeze directionals, got %q", got.Direction)
	}
	// NEUTRAL must not load the knife during a cascade.
	got = SelectDirection(RegimeContext{Regime: "RANGE", HurstValue: 0.4}, FundingContext{},
		EventContext{LiquidationCascade: true})
	if got.Direction != "WAIT" {
		t.Fatalf("cascade must freeze NEUTRAL (knife loading), got %q", got.Direction)
	}
	// LONG in TREND_UP stays frozen during a long-side cascade.
	got = SelectDirection(RegimeContext{Regime: "TREND_UP"}, FundingContext{},
		EventContext{LiquidationCascade: true})
	if got.Direction != "WAIT" {
		t.Fatalf("cascade must freeze LONG, got %q", got.Direction)
	}
}

func TestSelectDirectionFundingCarry(t *testing.T) {
	rng := RegimeContext{Regime: "RANGE", HurstValue: 0.45}
	// Stable positive carry in RANGE stays NEUTRAL (never short positive funding blindly).
	got := SelectDirection(rng, FundingContext{Avg48h: 0.0005, Stable48h: true}, EventContext{})
	if got.Direction != "NEUTRAL" {
		t.Fatalf("stable positive carry in RANGE must stay NEUTRAL, got %q (%s)", got.Direction, got.Reason)
	}
	// Stable negative carry → paid to hold long.
	got = SelectDirection(rng, FundingContext{Avg48h: -0.0005, Stable48h: true}, EventContext{})
	if got.Direction != "LONG" {
		t.Fatalf("stable negative carry must pick LONG, got %q", got.Direction)
	}
	// Unstable or trivial carry → neutral as before.
	got = SelectDirection(rng, FundingContext{Avg48h: 0.0005, Stable48h: false}, EventContext{})
	if got.Direction != "NEUTRAL" {
		t.Fatalf("unstable carry must stay NEUTRAL, got %q", got.Direction)
	}
	got = SelectDirection(rng, FundingContext{Avg48h: 0.0001, Stable48h: true}, EventContext{})
	if got.Direction != "NEUTRAL" {
		t.Fatalf("trivial carry must stay NEUTRAL, got %q", got.Direction)
	}
	// Extreme spot rate vetoes carry AND the whole RANGE deployment (v2.0.27
	// strengthened the old NEUTRAL fallback to WAIT — a directionless grid
	// must not carry one-sided inventory through a crowding window).
	got = SelectDirection(rng, FundingContext{Avg48h: 0.0005, Stable48h: true, IsExtreme: true}, EventContext{})
	if got.Direction != "WAIT" {
		t.Fatalf("extreme funding must veto carry and neutral, got %q", got.Direction)
	}
}

func TestBetaGateTrend(t *testing.T) {
	if down, up := betaGateTrend("TREND_DOWN", 30, -1.2); !down || up {
		t.Fatalf("BTC downtrend must gate down-only, got down=%v up=%v", down, up)
	}
	if down, up := betaGateTrend("TREND_UP", 30, 0.9); down || !up {
		t.Fatalf("BTC uptrend must gate up-only, got down=%v up=%v", down, up)
	}
	// Below the stricter activation band the gate stays off (DetectRegime
	// calls a trend at ADX≥22; the gate needs ≥20 plus a real slope).
	if down, up := betaGateTrend("TREND_DOWN", 18, -1.2); down || up {
		t.Fatalf("ADX 18 must not activate the beta gate, got down=%v up=%v", down, up)
	}
	if down, up := betaGateTrend("TREND_DOWN", 30, -0.1); down || up {
		t.Fatalf("flat slope must not activate the beta gate, got down=%v up=%v", down, up)
	}
	if down, up := betaGateTrend("RANGE", 40, 2.0); down || up {
		t.Fatalf("RANGE must never activate the gate, got down=%v up=%v", down, up)
	}
}

// v2.0.27: extreme funding of either sign vetoes NEUTRAL — a directionless
// grid cannot carry one-sided inventory through a crowding window (prod XMR
// #355 paid ~0.4%/day below mid on +0.131%/8h).
func TestSelectDirectionRangeExtremeFundingVetoesNeutral(t *testing.T) {
	decision := SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.6, HurstValue: 0.5},
		FundingContext{AverageRate: 0.0013, IsExtreme: true},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Fatalf("extreme funding in RANGE must WAIT, got %s (%s)", decision.Direction, decision.Reason)
	}
}

// The TREND branches keep their sign-aware logic: earning carry WITH a
// downtrend through an extreme POSITIVE rate is a trend-protected SHORT;
// the mirror (crowded paying shorts, extreme NEGATIVE) stays vetoed.
func TestSelectDirectionTrendDownFundingSignMatrix(t *testing.T) {
	carry := SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.7, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0013, IsExtreme: true},
		EventContext{},
	)
	if carry.Direction != "SHORT" {
		t.Fatalf("extreme POSITIVE funding in TREND_DOWN is a carry-earning SHORT, got %s (%s)", carry.Direction, carry.Reason)
	}
	squeeze := SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.7, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0013, IsExtreme: true},
		EventContext{},
	)
	if squeeze.Direction != "WAIT" {
		t.Fatalf("extreme NEGATIVE funding in TREND_DOWN must WAIT (squeeze fuel), got %s (%s)", squeeze.Direction, squeeze.Reason)
	}
}
