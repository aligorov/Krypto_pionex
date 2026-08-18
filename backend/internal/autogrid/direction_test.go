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
	if decision.Leverage != 2 {
		t.Errorf("expected leverage 2, got %d", decision.Leverage)
	}

	// TREND_DOWN + negative funding -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: -0.0005},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// TREND_DOWN + neutral funding -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0},
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
	if decision.Leverage != 2 {
		t.Errorf("expected leverage 2, got %d", decision.Leverage)
	}

	// TREND_UP + positive funding -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0005},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}

	// TREND_UP + neutral funding -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_UP", Confidence: 0.8, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
	}
}

func TestSelectDirectionRange(t *testing.T) {
	// RANGE + high confidence + low Hurst -> NEUTRAL
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

	// RANGE + low confidence -> WAIT
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.4, HurstValue: 0.42},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT, got %s (reason: %s)", decision.Direction, decision.Reason)
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

	// RANGE + confidence exactly at boundary (0.55) -> WAIT (strict >)
	decision = SelectDirection(
		RegimeContext{Regime: "RANGE", Confidence: 0.55, HurstValue: 0.42},
		FundingContext{AverageRate: 0.0},
		EventContext{},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT at confidence boundary, got %s (reason: %s)", decision.Direction, decision.Reason)
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

	// Liquidation cascade -> WAIT regardless of regime (even a perfect SHORT setup)
	decision = SelectDirection(
		RegimeContext{Regime: "TREND_DOWN", Confidence: 0.9, HurstValue: 0.7},
		FundingContext{AverageRate: 0.0005},
		EventContext{LiquidationCascade: true},
	)
	if decision.Direction != "WAIT" {
		t.Errorf("expected WAIT on liquidation cascade, got %s (reason: %s)", decision.Direction, decision.Reason)
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
