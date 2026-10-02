package autogrid

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

func TestComputeAdaptiveMeshDensityFromMargin(t *testing.T) {
	// A true 4% span around the price: (102−98)/100.
	lower := decimal.NewFromFloat(98)
	upper := decimal.NewFromFloat(102)
	price := decimal.NewFromFloat(100)
	budget := decimal.NewFromFloat(100)

	// v2.0.75 margin-density doctrine: $100×2x = $200 notional on a 4% span
	// → 7 levels × $28.60 (step ≈0.571%). v2.0.163: the density floor is the
	// fee-gate (3.6× round-trip at 5/2 bps = 0.504% ≈ 10× the 5 bps fee) —
	// the wide-step doctrine the survivor consensus calls for.
	res := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 5, 2)
	if res.GridNum != 7 {
		t.Errorf("expected 7 levels for $200 notional on a 4%% span, got %d", res.GridNum)
	}
	step, _ := res.GridStepPct.Float64()
	if step < 0.57 || step > 0.58 {
		t.Errorf("expected ~0.571%% step, got %.4f%%", step)
	}
	if perLevel := 200.0 / float64(res.GridNum); perLevel < marketdata.MinGridLevelNotionalUSDT {
		t.Errorf("every level must carry ≥ $8, got %.2f", perLevel)
	}

	// Thin notional widens the step to keep $8/level: $25×2x = $50 notional
	// on 4% → 6 levels × $8.33 (the operator's clamp-down case).
	thin := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", decimal.NewFromFloat(25), 2, 5, 2)
	if thin.GridNum != 6 {
		t.Errorf("expected 6 levels for $50 notional on a 4%% span, got %d", thin.GridNum)
	}
	// A high-ATR regime no longer sparses the grid — density follows margin.
	wild := ComputeAdaptiveMesh(lower, upper, price, 5.0, "VOLATILE", budget, 2, 5, 2)
	if wild.GridNum != res.GridNum {
		t.Errorf("regime/ATR must not change density anymore: %d vs %d", wild.GridNum, res.GridNum)
	}
	// Degenerate geometry keeps the 8-level fallback.
	degenerate := ComputeAdaptiveMesh(upper, lower, price, 0.5, "RANGE", budget, 2, 5, 2)
	if degenerate.GridNum != 8 {
		t.Errorf("degenerate geometry must fall back to 8 levels, got %d", degenerate.GridNum)
	}
}

// v2.0.93 FIX-A: the density floor follows the caller's fee/slippage pair —
// the same numbers the deploy-time fee-gate reads. At 20/10 bps the fee-gate
// bar is 1.50% (2.5× the 0.60% round trip since v2.0.94), so a 12% span that
// carries 25 levels at the 5/2 fleet default collapses to 8; the operator
// raising fees shrinks the fleet's density ceiling in lockstep instead of
// starving it at a gate the floor no longer matches.
func TestComputeAdaptiveMeshFloorFollowsFees(t *testing.T) {
	lower := decimal.NewFromFloat(94)
	upper := decimal.NewFromFloat(106)
	price := decimal.NewFromFloat(100)
	budget := decimal.NewFromFloat(100)

	defaults := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 5, 2)
	if defaults.GridNum != 23 {
		// $200 notional on a 12% span: the 3.6× fee floor binds
		// (0.504% step) → floor(12/0.504) = 23 levels ($8-step is 0.48%).
		t.Fatalf("5/2 bps on a 12%% span at $200 notional = 23 levels, got %d", defaults.GridNum)
	}
	pricy := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 20, 10)
	if pricy.GridNum != 6 {
		t.Fatalf("20/10 bps bar 2.16%% → 6 levels on a 12%% span, got %d", pricy.GridNum)
	}
	step, _ := pricy.GridStepPct.Float64()
	if step < 2.0 {
		t.Fatalf("pricy-fee step must clear toward the 2.16%% fee-gate bar, got %.4f%%", step)
	}
	if pricy.GridNum >= defaults.GridNum {
		t.Fatalf("pricier fees must thin the grid: %d vs %d", pricy.GridNum, defaults.GridNum)
	}
	// Unknown costs (≤0) degrade to the documented fleet-default floor.
	unknown := ComputeAdaptiveMesh(lower, upper, price, 0.50, "RANGE", budget, 2, 0, 0)
	if unknown.GridNum != defaults.GridNum {
		t.Fatalf("unknown fees must fall back to the default floor (%d levels), got %d",
			defaults.GridNum, unknown.GridNum)
	}
}

// v2.0.94 quality upgrade: a wide span (≥14%) on a quiet tape (ATR ≤2.5%)
// takes ONE notch above base (cap 6) — the weekly mining put ZERO stop-outs
// in the ≥14% span cohort while every stop of the week lived in ≤9% spans.
// The narrow-span de-gir must keep priority, and a noisy tape must NOT
// upgrade even on a wide span.
func TestComputeDynamicLeverageQualityUpgrade(t *testing.T) {
	up := ComputeDynamicLeverage(2.0, 4, 16.0)
	if up.Leverage != 5 || up.IsScaleDown {
		t.Fatalf("wide quiet span must upgrade 4x→5x, got %dx (%s)", up.Leverage, up.Reason)
	}
	// Hard cap at 6x even with a higher base.
	capped := ComputeDynamicLeverage(1.0, 6, 20.0)
	if capped.Leverage != 6 {
		t.Fatalf("upgrade must cap at 6x, got %d", capped.Leverage)
	}
	// Noisy tape on a wide span: stays at base.
	noisy := ComputeDynamicLeverage(3.5, 4, 16.0)
	if noisy.Leverage != 4 {
		t.Fatalf("ATR 3.5%% must not upgrade, got %d", noisy.Leverage)
	}
	// Narrow span still de-gears — the de-gir branch runs first.
	degear := ComputeDynamicLeverage(2.0, 4, 5.0)
	if degear.Leverage != 2 || !degear.IsScaleDown {
		t.Fatalf("narrow span must still de-gear to 2x, got %d", degear.Leverage)
	}
	// Mid-zone span (7-14%): no upgrade, base leverage.
	mid := ComputeDynamicLeverage(2.0, 4, 10.0)
	if mid.Leverage != 4 {
		t.Fatalf("mid-zone span must keep base 4x, got %d", mid.Leverage)
	}
}

// v2.0.93 FIX-I: the ±2% boundary clamp — one helper shared by the REAL
// deploy, the paper deploy and both DGT re-center arms. A degenerate (near
// zero) ATR parks the raw stop inside the range; the clamp pushes it back
// outside the bounds the grid actually trades.
func TestClampAntiHuntStopIntoBounds(t *testing.T) {
	lower := decimal.NewFromFloat(98)
	upper := decimal.NewFromFloat(102)

	// Degenerate LONG stop INSIDE the range → clamped to lower×0.98.
	inside := decimal.NewFromFloat(99)
	got := ClampAntiHuntStopIntoBounds("LONG", lower, upper, inside)
	if want := decimal.NewFromFloat(96.04); !got.Equal(want) {
		t.Fatalf("LONG stop inside the range must clamp to lower×0.98 = %s, got %s", want, got)
	}
	got = ClampAntiHuntStopIntoBounds("NEUTRAL", lower, upper, inside)
	if want := decimal.NewFromFloat(96.04); !got.Equal(want) {
		t.Fatalf("NEUTRAL stop inside the range must clamp like LONG, want %s got %s", want, got)
	}
	// Degenerate SHORT stop inside the range → clamped to upper×1.02.
	got = ClampAntiHuntStopIntoBounds("SHORT", lower, upper, decimal.NewFromFloat(101))
	if want := decimal.NewFromFloat(104.04); !got.Equal(want) {
		t.Fatalf("SHORT stop inside the range must clamp to upper×1.02 = %s, got %s", want, got)
	}
	// A healthy stop OUTSIDE the bounds passes through untouched on both
	// sides — the clamp must not move well-formed stops.
	healthyLong := decimal.NewFromFloat(95)
	if got = ClampAntiHuntStopIntoBounds("LONG", lower, upper, healthyLong); !got.Equal(healthyLong) {
		t.Fatalf("healthy LONG stop must pass through, got %s", got)
	}
	healthyShort := decimal.NewFromFloat(105)
	if got = ClampAntiHuntStopIntoBounds("SHORT", lower, upper, healthyShort); !got.Equal(healthyShort) {
		t.Fatalf("healthy SHORT stop must pass through, got %s", got)
	}
}

// v2.0.93 FIX-K: the AI Kit adoption clamp folds the exchange's advisory
// count into the margin-density doctrine — ceiling = the doctrine count for
// the span (fee-gate floor at the actual fees + the $8/level notional cap),
// floor = the doctrine's 6 levels.
func TestClampAIGridCount(t *testing.T) {
	// A 4% span at $200 notional, 5/2 bps: doctrine count = 7 (v2.0.163
	// 3.6× floor). A spot AI count of 150 clamps down to 7 (born viable
	// instead of born rejected).
	if got := clampAIGridCount(4, 200, 5, 2, 150); got != 7 {
		t.Fatalf("spot AI 150 over a 4%% span must clamp to the doctrine ceiling 7, got %d", got)
	}
	// Pricier fees shrink the ceiling in lockstep (FIX-A parity): 20/10 bps
	// → floor 1.50% → the 4% span clamps to the 6-level grid floor.
	if got := clampAIGridCount(4, 200, 20, 10, 150); got != 6 {
		t.Fatalf("pricy fees must collapse the ceiling to the 6-level floor, got %d", got)
	}
	// FIX-K: a tiny AI count (spot grid of 2) adopts UP to the doctrine
	// floor of 6 — the old clamp accepted 2.
	if got := clampAIGridCount(4, 200, 5, 2, 2); got != 6 {
		t.Fatalf("a 2-level AI adoption must lift to the doctrine floor 6, got %d", got)
	}
	// $8/level notional cap binds for cheap fees: 12% span, $200 notional,
	// 2/1 bps → doctrine count 25 regardless of the AI's 400-level spot grid.
	if got := clampAIGridCount(12, 200, 2, 1, 400); got != 25 {
		t.Fatalf("the $8/level notional cap must bound adoption at 25, got %d", got)
	}
	// A count inside the window is adopted verbatim.
	if got := clampAIGridCount(12, 200, 2, 1, 20); got != 20 {
		t.Fatalf("an in-window AI count must be adopted verbatim, got %d", got)
	}
}

func TestComputeDynamicLeverage(t *testing.T) {
	// Normal volatility (ATR 1.5%) -> full base leverage 4x
	resNormal := ComputeDynamicLeverage(1.5, 4, 8.0)
	if resNormal.Leverage != 4 || resNormal.IsScaleDown {
		t.Errorf("expected leverage 4 with no scale down, got %d (scale down: %v)", resNormal.Leverage, resNormal.IsScaleDown)
	}

	// Elevated volatility (ATR 5.5%) -> 3x
	resElevated := ComputeDynamicLeverage(5.5, 4, 8.0)
	if resElevated.Leverage != 3 || !resElevated.IsScaleDown {
		t.Errorf("expected leverage 3 with scale down, got %d (scale down: %v)", resElevated.Leverage, resElevated.IsScaleDown)
	}

	// Extreme volatility (ATR 12.0%) -> 2x
	resExtreme := ComputeDynamicLeverage(12.0, 4, 8.0)
	if resExtreme.Leverage != 2 || !resExtreme.IsScaleDown {
		t.Errorf("expected leverage 2 with scale down, got %d (scale down: %v)", resExtreme.Leverage, resExtreme.IsScaleDown)
	}
}

func TestComputeAntiHuntStop(t *testing.T) {
	lower := decimal.NewFromFloat(10.0)
	upper := decimal.NewFromFloat(12.0)
	price := decimal.NewFromFloat(10.5)
	atr := decimal.NewFromFloat(0.20) // 2% ATR

	stopLong := ComputeAntiHuntStop("LONG", lower, upper, price, atr, 1.5)
	// Stop should be lower - (1.5 * 0.20) = 10.0 - 0.30 = 9.70
	expected := decimal.NewFromFloat(9.70)
	if !stopLong.Equal(expected) {
		t.Errorf("expected anti hunt stop %s, got %s", expected, stopLong)
	}

	stopShort := ComputeAntiHuntStop("SHORT", lower, upper, price, atr, 1.5)
	// Stop should be upper + 0.30 = 12.30
	expectedShort := decimal.NewFromFloat(12.30)
	if !stopShort.Equal(expectedShort) {
		t.Errorf("expected anti hunt stop short %s, got %s", expectedShort, stopShort)
	}
}

// v2.0.52 narrow-span de-gear: spans <7% cap leverage at 2x — the audit
// showed a 4x maxLoss stop 2% from entry inside one daily sigma.
func TestComputeDynamicLeverageNarrowSpan(t *testing.T) {
	res := ComputeDynamicLeverage(1.0, 4, 2.8) // LINK-class span
	if res.Leverage != 2 || !res.IsScaleDown {
		t.Fatalf("2.8%% span at base 4x must de-gear to 2x, got %dx (%s)", res.Leverage, res.Reason)
	}
	res = ComputeDynamicLeverage(1.0, 2, 3.5)
	if res.Leverage != 2 || res.IsScaleDown {
		t.Fatalf("base 2x stays 2x without a scale-down flag, got %dx", res.Leverage)
	}
	res = ComputeDynamicLeverage(1.0, 4, 6.9)
	if res.Leverage != 2 {
		t.Fatalf("6.9%% span must de-gear to 2x, got %dx", res.Leverage)
	}
	res = ComputeDynamicLeverage(1.0, 4, 7.0)
	if res.Leverage != 4 {
		t.Fatalf("7.0%% span boundary keeps base leverage, got %dx", res.Leverage)
	}
	res = ComputeDynamicLeverage(1.0, 4, 0) // span unknown
	if res.Leverage != 4 {
		t.Fatalf("unknown span (0) must not de-gear, got %dx", res.Leverage)
	}
	// ATR de-gear still stacks correctly on wide spans.
	res = ComputeDynamicLeverage(12.0, 4, 18.0)
	if res.Leverage != 2 {
		t.Fatalf("extreme ATR on wide span must scale to 2x, got %dx", res.Leverage)
	}
}

// v2.0.181 densify-after-widen: prod MUX #1530 shipped 6 rows stretched
// over the 8.0% widened span (1.4% step, 2.7x the 0.504% golden band) —
// the count must follow the FINAL span, raise-only.
func TestDensifyGridNumForSpan(t *testing.T) {
	price := decimal.NewFromFloat(1049.19)
	lower := decimal.NewFromFloat(1007.42)
	upper := decimal.NewFromFloat(1091.38) // MUX #1530 exact bounds, ~8.0% span

	// Born-narrow count densifies into the golden band: 6 → 15 at
	// $75×4x notional (step floor 0.504% governs, $20/level).
	got := DensifyGridNumForSpan(6, lower, upper, price, 300, 5, 2)
	if got < 14 || got > 16 {
		t.Fatalf("MUX geometry must densify 6 into the 14-16 band, got %d", got)
	}
	step := 8.0 / float64(got)
	if step < 0.504-1e-9 || step > 0.6 {
		t.Fatalf("densified step %.3f%% must land back in the golden band", step)
	}

	// Raise-only: a count already denser than the doctrine (INJ-class born
	// wide, 16) is never thinned.
	if got := DensifyGridNumForSpan(20, lower, upper, price, 300, 5, 2); got != 20 {
		t.Fatalf("born-dense count must never thin, got %d", got)
	}

	// Un-widened narrow span: the doctrine count stays BELOW the born floor
	// clamp — no change (the 6 stays the doctrine's own minimum).
	narrowLower := decimal.NewFromFloat(1030.83)
	narrowUpper := decimal.NewFromFloat(1067.55) // ~3.5% span
	if got := DensifyGridNumForSpan(6, narrowLower, narrowUpper, price, 300, 5, 2); got != 6 {
		t.Fatalf("narrow-span born count must keep, got %d", got)
	}

	// Small notional: the per-level $8 floor widens the min-order step and
	// caps the densified count (span 8% / $100 → step 0.64% → 12 rows).
	if got := DensifyGridNumForSpan(6, lower, upper, price, 100, 5, 2); got != 12 {
		t.Fatalf("$100 notional must densify to 12 rows ($8.33/level), got %d", got)
	}

	// Degenerate inputs: count unchanged.
	if got := DensifyGridNumForSpan(6, upper, lower, price, 300, 5, 2); got != 6 {
		t.Fatalf("inverted bounds must not touch the count, got %d", got)
	}
	if got := DensifyGridNumForSpan(6, lower, upper, decimal.Zero, 300, 5, 2); got != 6 {
		t.Fatalf("zero price must not touch the count, got %d", got)
	}
}

// v2.0.182: S/R geometry drifts a few bps per scan — the params comparator
// must match a DONE job inside a 0.2% band (below half a grid step), or the
// gate re-enqueues forever (prod 01.10: MSTRX 15 jobs / 15 distinct params).
func TestMatchesDeployParamsDriftTolerance(t *testing.T) {
	p := &BacktestDeployParams{
		LowerPrice: decimal.NewFromFloat(1030.83),
		UpperPrice: decimal.NewFromFloat(1067.55),
		GridNum:    15, Leverage: 4,
	}
	mk := func(lo, up float64, grid int) []byte {
		b, _ := json.Marshal(map[string]any{
			"lower_price": lo, "upper_price": up, "grid_num": grid, "leverage": 4,
		})
		return b
	}
	// Drift well inside the band matches.
	if !matchesDeployParams(mk(1030.5, 1067.9, 15), p) {
		t.Fatal("0.03% drift must match the cached job")
	}
	// Drift beyond 0.2% rejects (that is a materially different grid).
	if matchesDeployParams(mk(1028.0, 1067.55, 15), p) {
		t.Fatal("0.27% lower drift must NOT match")
	}
	// Grid count must stay exact.
	if matchesDeployParams(mk(1030.83, 1067.55, 16), p) {
		t.Fatal("different grid_num must NOT match")
	}
}

// v2.0.183: the span floor follows the pair's own daily noise — grids stop
// collapsing onto the flat 8%, and high-vol pairs earn wider, denser grids.
func TestDeploySpanFloorVolScaled(t *testing.T) {
	cases := []struct{ vol, want float64 }{
		{0, 8},      // unknown vol → flat doctrine floor
		{2.6, 8},    // quiet pair → floor
		{4.0, 8},    // 2σ = 8% → exactly the floor
		{5.7, 11.4}, // SNXXX-class: 2σ above the floor
		{8.2, 16.4}, // WLD-class
		{20.0, 25},  // clamped at the doctrine cap
	}
	for _, c := range cases {
		if got := DeploySpanFloorPct(c.vol); math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("DeploySpanFloorPct(%v) = %v, want %v", c.vol, got, c.want)
		}
	}
	// WLD-class widening: 5% span on vol 8.2% must widen to ~16.4%.
	price := decimal.NewFromFloat(0.4884)
	lower := decimal.NewFromFloat(0.4762)
	upper := decimal.NewFromFloat(0.5006)
	nl, nu := EnsureDeploySpanForVol(lower, upper, price, 8.2)
	span, _ := nu.Sub(nl).Div(price).Mul(decimal.NewFromInt(100)).Float64()
	if span < 16.3 || span > 16.5 {
		t.Fatalf("vol 8.2%% must widen to ~16.4%% span, got %.2f%%", span)
	}
	// Already-wider span is never shrunk.
	wl, wu := EnsureDeploySpanForVol(nl, nu, price, 2.0)
	if !wl.Equal(nl) || !wu.Equal(nu) {
		t.Fatal("wider span must not shrink")
	}
	// Densify follows the vol span: 16.4% / 0.504% → 32 rows at $75×4x.
	if rows := DensifyGridNumForSpan(15, nl, nu, price, 300, 5, 2); rows < 31 || rows > 33 {
		t.Fatalf("16.4%% span must densify to 31-33 rows, got %d", rows)
	}
}

// v2.0.183: the stress ceiling reads the ACTUAL span.
func TestBotGridSpanPct(t *testing.T) {
	price := decimal.NewFromFloat(1049.19)
	lower := decimal.NewFromFloat(1007.42)
	upper := decimal.NewFromFloat(1091.38)
	if got := botGridSpanPct(lower, upper, price); got < 7.9 || got > 8.1 {
		t.Fatalf("MUX span must read ~8%%, got %.2f", got)
	}
	if got := botGridSpanPct(upper, lower, price); got != 0 {
		t.Fatalf("inverted bounds must read 0, got %.2f", got)
	}
}

// v2.0.186 pre-break regime exit: prod ZAMA #1537 sat underwater at the
// top edge of its range in a CONFIRMED TREND_UP for 1.5h before the
// physical break; the early exit cuts it at the edge instead.
func TestNeutralPreBreakRegimeExit(t *testing.T) {
	base := func(price, total float64, regime string) botActionInput {
		totalD := decimal.NewFromFloat(total)
		return botActionInput{
			Direction: "NEUTRAL", Regime: regime,
			Lower: decimal.NewFromFloat(100), Upper: decimal.NewFromFloat(110),
			CurrentPrice: decimal.NewFromFloat(price),
			RealizedPNL: decimal.Zero, UnrealizedPNL: totalD, PeakPNL: totalD,
			Budget: decimal.NewFromInt(75),
			PnLTarget: decVal(1.5), MaxLoss: decVal(30),
			RangeBreakBuffer: decimal.NewFromInt(2),
		}
	}
	// ZAMA shape: TREND_UP, 89% of the range, underwater -> early exit.
	d := decideBotAction(base(108.9, -3.0, "TREND_UP"))
	if d.Action != ActionCloseRangeBreak || d.Reason != "RANGE_BREAK_UP_EARLY" {
		t.Fatalf("underwater neutral at the up edge in TREND_UP must exit early, got %+v", d)
	}
	// Same position, but IN PROFIT -> the normal TP path owns it.
	d = decideBotAction(base(108.9, 1.0, "TREND_UP"))
	if d.Action == ActionCloseRangeBreak && d.Reason == "RANGE_BREAK_UP_EARLY" {
		t.Fatal("profitable neutral must not take the early loss exit")
	}
	// TREND_UP but mid-range -> not an edge, hold the line.
	d = decideBotAction(base(105.0, -3.0, "TREND_UP"))
	if d.Action == ActionCloseRangeBreak && d.Reason == "RANGE_BREAK_UP_EARLY" {
		t.Fatal("mid-range neutral must not early-exit")
	}
	// Mirror: TREND_DOWN, 10% of the range, underwater -> early exit down.
	d = decideBotAction(base(101.0, -3.0, "TREND_DOWN"))
	if d.Action != ActionCloseRangeBreak || d.Reason != "RANGE_BREAK_DOWN_EARLY" {
		t.Fatalf("underwater neutral at the down edge in TREND_DOWN must exit early, got %+v", d)
	}
	// RANGE regime -> the classic border logic only.
	d = decideBotAction(base(108.9, -3.0, "RANGE"))
	if d.Action == ActionCloseRangeBreak && strings.HasPrefix(d.Reason, "RANGE_BREAK_UP_EARLY") {
		t.Fatal("RANGE regime must not trigger the pre-break exit")
	}
}

func decVal(v float64) decimal.Decimal {
	return decimal.NewFromFloat(v)
}

// v2.0.189: directionals get the pre-break save too — a LONG riding a
// confirmed TREND_DOWN at the floor of its range exits before the break
// (prod MSTRX #1546 class).
func TestDirectionalPreBreakRegimeExit(t *testing.T) {
	mk := func(direction string, price, total float64, regime string) botActionInput {
		totalD := decimal.NewFromFloat(total)
		return botActionInput{
			Direction: direction, Regime: regime,
			Lower: decimal.NewFromFloat(100), Upper: decimal.NewFromFloat(110),
			CurrentPrice: decimal.NewFromFloat(price),
			RealizedPNL: decimal.Zero, UnrealizedPNL: totalD, PeakPNL: totalD,
			Budget: decimal.NewFromInt(75),
			PnLTarget: decVal(1.5), MaxLoss: decVal(16),
			RangeBreakBuffer: decimal.NewFromInt(2),
		}
	}
	// LONG underwater at 10% of the range in TREND_DOWN -> early exit.
	if d := decideBotAction(mk("LONG", 101.0, -5.0, "TREND_DOWN")); d.Action != ActionCloseRangeBreak || d.Reason != "RANGE_BREAK_DOWN_EARLY" {
		t.Fatalf("underwater LONG at the down edge in TREND_DOWN must exit early, got %+v", d)
	}
	// LONG in profit -> the normal path owns it.
	if d := decideBotAction(mk("LONG", 101.0, 2.0, "TREND_DOWN")); d.Action == ActionCloseRangeBreak && d.Reason == "RANGE_BREAK_DOWN_EARLY" {
		t.Fatal("profitable LONG must not take the early loss exit")
	}
	// LONG underwater mid-range -> hold the line.
	if d := decideBotAction(mk("LONG", 105.0, -5.0, "TREND_DOWN")); d.Action == ActionCloseRangeBreak && d.Reason == "RANGE_BREAK_DOWN_EARLY" {
		t.Fatal("mid-range LONG must not early-exit")
	}
	// SHORT underwater at the ceiling in TREND_UP -> early exit (mirror).
	if d := decideBotAction(mk("SHORT", 108.9, -5.0, "TREND_UP")); d.Action != ActionCloseRangeBreak || d.Reason != "RANGE_BREAK_UP_EARLY" {
		t.Fatalf("underwater SHORT at the up edge in TREND_UP must exit early, got %+v", d)
	}
}
