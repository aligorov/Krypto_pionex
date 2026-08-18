package autogrid

import (
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func mustDecimal(value string) decimal.Decimal {
	result, err := decimal.NewFromString(value)
	if err != nil {
		panic(err)
	}
	return result
}

func baseActionInput() botActionInput {
	return botActionInput{
		Direction:        "NEUTRAL",
		Lower:            mustDecimal("100"),
		Upper:            mustDecimal("120"),
		CurrentPrice:     mustDecimal("110"),
		RealizedPNL:      decimal.Zero,
		UnrealizedPNL:    decimal.Zero,
		PnLTarget:        mustDecimal("12"),
		MaxLoss:          mustDecimal("8"),
		RangeBreakBuffer: mustDecimal("1"),
		AdjustmentsLeft:  3,
		Regime:           "RANGE",
	}
}

func TestDecideBotActionTakeProfit(t *testing.T) {
	input := baseActionInput()
	input.RealizedPNL = mustDecimal("12")
	decision := decideBotAction(input)
	if decision.Action != ActionCloseTakeProfit || decision.Reason != "TAKE_PROFIT" {
		t.Fatalf("expected native take-profit close, got %+v", decision)
	}
}

func TestDecideBotActionTrailingTakeProfit(t *testing.T) {
	input := baseActionInput() // Target is 12
	// Bot previously hit 10 USDT (83% of target), but now pulled back to 7.5 USDT (< 8.0 trailing floor)
	input.PeakPNL = mustDecimal("10")
	input.RealizedPNL = mustDecimal("7.5")
	decision := decideBotAction(input)
	if decision.Action != ActionCloseTakeProfit || decision.Reason != "TRAILING_TAKE_PROFIT" {
		t.Fatalf("expected trailing take-profit close on pullback from peak, got %+v", decision)
	}
}

func TestDecideBotActionBreakevenLock(t *testing.T) {
	input := baseActionInput() // Target is 12, Budget is 200
	input.Budget = mustDecimal("200")
	// Bot previously hit 6.5 USDT (54% of target), but now dropped to 0.1 USDT (<= 0.4 USDT breakeven floor)
	input.PeakPNL = mustDecimal("6.5")
	input.RealizedPNL = mustDecimal("0.1")
	decision := decideBotAction(input)
	if decision.Action != ActionCloseTakeProfit || decision.Reason != "BREAKEVEN_LOCK" {
		t.Fatalf("expected breakeven lock close, got %+v", decision)
	}
}

func TestDecideBotActionStopLoss(t *testing.T) {
	input := baseActionInput()
	input.UnrealizedPNL = mustDecimal("-8")
	decision := decideBotAction(input)
	if decision.Action != ActionCloseStopLoss || decision.Reason != "STOP_LOSS" {
		t.Fatalf("expected stop-loss close, got %+v", decision)
	}
}

func TestDecideBotActionHoldInsideRange(t *testing.T) {
	decision := decideBotAction(baseActionInput())
	if decision.Action != ActionHold {
		t.Fatalf("expected hold, got %+v", decision)
	}
}

func TestDecideBotActionLongDownsideBreakTrendCloses(t *testing.T) {
	input := baseActionInput()
	input.Direction = "LONG"
	input.CurrentPrice = mustDecimal("98") // below 100 * 0.99
	input.Regime = "TREND_DOWN"
	decision := decideBotAction(input)
	if decision.Action != ActionCloseRangeBreak {
		t.Fatalf("expected close on downside break in downtrend, got %+v", decision)
	}
}

func TestDecideBotActionLongDownsideBreakRangeShifts(t *testing.T) {
	input := baseActionInput()
	input.Direction = "LONG"
	input.CurrentPrice = mustDecimal("98")
	input.Regime = "RANGE"
	decision := decideBotAction(input)
	if decision.Action != ActionAdjustDown {
		t.Fatalf("expected range shift down in RANGE regime, got %+v", decision)
	}
	width := input.Upper.Sub(input.Lower)
	expectedLower := input.CurrentPrice.Sub(width.Div(decimal.NewFromInt(2)))
	if !decision.NewLower.Equal(expectedLower) {
		t.Fatalf("expected shifted lower %s, got %s", expectedLower, decision.NewLower)
	}
}

func TestDecideBotActionNoAdjustmentsLeftCloses(t *testing.T) {
	input := baseActionInput()
	input.Direction = "LONG"
	input.CurrentPrice = mustDecimal("98")
	input.Regime = "RANGE"
	input.AdjustmentsLeft = 0
	decision := decideBotAction(input)
	if decision.Action != ActionCloseRangeBreak {
		t.Fatalf("expected close when adjustments exhausted, got %+v", decision)
	}
}

func TestDecideBotActionShortUpsideBreakClosesInUptrend(t *testing.T) {
	input := baseActionInput()
	input.Direction = "SHORT"
	input.CurrentPrice = mustDecimal("122") // above 120 * 1.01
	input.Regime = "TREND_UP"
	decision := decideBotAction(input)
	if decision.Action != ActionCloseRangeBreak {
		t.Fatalf("expected close on upside break against short, got %+v", decision)
	}
}

func TestDecideBotActionLongUpsideBreakFollowsWithShift(t *testing.T) {
	input := baseActionInput()
	input.Direction = "LONG"
	input.CurrentPrice = mustDecimal("122")
	input.Regime = "TREND_UP"
	decision := decideBotAction(input)
	if decision.Action != ActionAdjustUp {
		t.Fatalf("expected range shift up for long in uptrend, got %+v", decision)
	}
	if !decision.NewUpper.Equal(mustDecimal("132")) {
		t.Fatalf("expected new upper 132, got %s", decision.NewUpper)
	}
}

func TestDecideBotActionTargetsDisabled(t *testing.T) {
	input := baseActionInput()
	input.PnLTarget = decimal.Zero
	input.MaxLoss = decimal.Zero
	input.RealizedPNL = mustDecimal("500")
	input.UnrealizedPNL = mustDecimal("-500")
	if decision := decideBotAction(input); decision.Action != ActionHold {
		t.Fatalf("expected hold with targets disabled, got %+v", decision)
	}
}

func TestTerminalOutcomeMapping(t *testing.T) {
	cases := map[string][2]string{
		"profit_stop":        {"COMPLETED", "TAKE_PROFIT_NATIVE"},
		"loss_stop":          {"STOPPED", "STOP_LOSS_NATIVE"},
		"user_cancel":        {"STOPPED", "USER_CANCEL"},
		"liquidated":         {"LIQUIDATED", "LIQUIDATION"},
		"not_enough_balance": {"FAILED", "REMOTE_FAILED"},
		"":                   {"STOPPED", "EXTERNAL_CLOSE"},
	}
	for reason, expected := range cases {
		status, closedReason := terminalOutcome(reason)
		if status != expected[0] || closedReason != expected[1] {
			t.Fatalf("reason %q: expected %v, got %s/%s", reason, expected, status, closedReason)
		}
	}
}

func TestSplitPionexPerp(t *testing.T) {
	base, quote, err := SplitPionexPerp("BTC_USDT_PERP")
	if err != nil || base != "BTC" || quote != "USDT" {
		t.Fatalf("expected BTC/USDT, got %s/%s (%v)", base, quote, err)
	}
	if _, _, err := SplitPionexPerp("BTCUSDT"); err == nil {
		t.Fatal("expected error for non-PERP symbol")
	}
}

func TestAIAdaptedRange(t *testing.T) {
	lower, upper := AIAdaptedRange(
		mustDecimal("110"), mustDecimal("90"), mustDecimal("100"),
	)
	if !lower.GreaterThan(decimal.Zero) || !upper.GreaterThan(lower) {
		t.Fatalf("expected adapted range around price, got [%s, %s]", lower, upper)
	}
	// Width must be preserved relative to the AI recommendation (±10%).
	if lower.GreaterThan(mustDecimal("91")) || upper.LessThan(mustDecimal("109")) {
		t.Fatalf("expected AI width preserved, got [%s, %s]", lower, upper)
	}

	// Extreme AI width is clamped to the ±12.5% safety band.
	lower, upper = AIAdaptedRange(
		mustDecimal("200"), mustDecimal("50"), mustDecimal("100"),
	)
	if lower.LessThan(mustDecimal("87.4")) || upper.GreaterThan(mustDecimal("112.6")) {
		t.Fatalf("expected clamped range, got [%s, %s]", lower, upper)
	}

	// Degenerate input yields zeros so callers fall back to the scanner range.
	lower, upper = AIAdaptedRange(
		mustDecimal("90"), mustDecimal("110"), decimal.Zero,
	)
	if !lower.IsZero() || !upper.IsZero() {
		t.Fatalf("expected zero fallback, got [%s, %s]", lower, upper)
	}
}

func TestMergePresetKeepsOperatorFields(t *testing.T) {
	current := Settings{
		AccountID:     testStringPtr("account-1"),
		ExecutionMode: "REAL",
		BudgetUSDT:    mustDecimal("777"),
		Leverage:      5, MaxActiveBots: 3,
		MinSharpe: mustDecimal("0.1"), MinEVPct: mustDecimal("0.2"),
		PnLTargetUSDT: mustDecimal("1"), MaxLossUSDT: mustDecimal("2"),
	}
	preset := MarketPhasePresets()[0] // flat_harvester
	input := mergePreset(current, preset)

	if input.ExecutionMode != "REAL" || input.AccountID == nil || *input.AccountID != "account-1" {
		t.Fatal("preset must not touch execution mode or account")
	}
	if !input.BudgetUSDT.Equal(mustDecimal("777")) {
		t.Fatalf("preset must not touch budget, got %s", input.BudgetUSDT)
	}
	if input.Leverage != 2 || input.MaxActiveBots != 3 {
		t.Fatalf("patch not applied: leverage=%d maxActiveBots=%d", input.Leverage, input.MaxActiveBots)
	}
	if input.PnLTargetMode != "DYNAMIC" {
		t.Fatalf("preset must switch PnL mode to DYNAMIC, got %q", input.PnLTargetMode)
	}
	// Fixed amounts are kept as the FIXED-mode fallback, not overwritten.
	if !input.PnLTargetUSDT.Equal(mustDecimal("1")) || !input.MaxLossUSDT.Equal(mustDecimal("2")) {
		t.Fatalf("fixed fallback amounts must survive: target=%s loss=%s", input.PnLTargetUSDT, input.MaxLossUSDT)
	}
}

func TestAllPresetsFitValidation(t *testing.T) {
	for _, preset := range MarketPhasePresets() {
		input := mergePreset(Settings{
			ExecutionMode: "PAPER", BudgetUSDT: mustDecimal("100"),
			MinSharpe: mustDecimal("0.1"), MinEVPct: mustDecimal("0"),
			FeeBps: mustDecimal("5"), SlippageBps: mustDecimal("2"),
			MinVolume24h:     mustDecimal("500000"),
			MinVolatilityPct: mustDecimal("1"), MaxVolatilityPct: mustDecimal("40"),
			MaxDrawdownPct: mustDecimal("30"), MinProfitFactor: mustDecimal("1"),
			PnLTargetUSDT: mustDecimal("0"), MaxLossUSDT: mustDecimal("0"),
			RangeBreakBufferPct: mustDecimal("1"),
			StopLossMode:        "NONE", CandleInterval: "60M",
		}, preset)
		if input.Leverage < 1 || input.Leverage > 100 {
			t.Fatalf("%s: leverage %d out of bounds", preset.ID, input.Leverage)
		}
		if input.MaxActiveBots < 1 || input.MaxActiveBots > 20 {
			t.Fatalf("%s: maxActiveBots %d out of bounds", preset.ID, input.MaxActiveBots)
		}
		if !(input.MinVolatilityPct.LessThan(input.MaxVolatilityPct)) {
			t.Fatalf("%s: volatility band inverted", preset.ID)
		}
		if input.ManageIntervalSeconds < 15 || input.ManageIntervalSeconds > 86400 {
			t.Fatalf("%s: manage interval %d out of bounds", preset.ID, input.ManageIntervalSeconds)
		}
		if input.RangeBreakBufferPct.LessThanOrEqual(decimal.Zero) ||
			input.RangeBreakBufferPct.GreaterThan(mustDecimal("20")) {
			t.Fatalf("%s: buffer %s out of bounds", preset.ID, input.RangeBreakBufferPct)
		}
		if input.MaxAdjustmentsPerBot < 0 || input.MaxAdjustmentsPerBot > 10 {
			t.Fatalf("%s: adjustments %d out of bounds", preset.ID, input.MaxAdjustmentsPerBot)
		}
	}
}

func testStringPtr(value string) *string { return &value }

func TestNeutralGridPaperPNLPairsEarnOnlyOnReturn(t *testing.T) {
	lower, upper := mustDecimal("100"), mustDecimal("120")
	investment := mustDecimal("200")
	feeBps := mustDecimal("5")
	// A one-way traverse out of the range must book ZERO realized profit:
	// Pionex attributes grid profit to completed buy/sell pairs only, and a
	// one-way dump just loads inventory (the v1.3.21-and-earlier model booked
	// phantom half round trips on the way down).
	profit, _, _ := neutralGridPaperPNL(lower, upper, 20, investment, 1, 10, 2, mustDecimal("102"), feeBps)
	if !profit.IsZero() {
		t.Fatalf("one-way down traverse must earn nothing, got %s", profit)
	}
	// The return leg closes the accumulated inventory: 8 completed pairs at
	// lev 1 → perLevel 10 × (step 1/110 − fees 0.001) × 8 ≈ 0.6473.
	profit, _, _ = neutralGridPaperPNL(lower, upper, 20, investment, 1, 2, 10, mustDecimal("110"), feeBps)
	expected := mustDecimal("0.6473")
	if profit.Sub(expected).Abs().GreaterThan(mustDecimal("0.0001")) {
		t.Fatalf("return leg must earn 8 completed pairs ≈ 0.6473, got %s", profit)
	}
	// No movement, no profit.
	profit, _, _ = neutralGridPaperPNL(lower, upper, 20, investment, 1, 7, 7, mustDecimal("110"), feeBps)
	if !profit.IsZero() {
		t.Fatalf("zero crossings must earn nothing, got %s", profit)
	}
	// Extending inventory further past an already-loaded side earns nothing.
	profit, _, _ = neutralGridPaperPNL(lower, upper, 20, investment, 1, 14, 17, mustDecimal("107"), feeBps)
	if !profit.IsZero() {
		t.Fatalf("crossing away from inventory must earn nothing, got %s", profit)
	}
}

func TestNeutralGridPaperPNLInventoryBelowMid(t *testing.T) {
	lower, upper := mustDecimal("100"), mustDecimal("120")
	investment := mustDecimal("200")
	feeBps := mustDecimal("5")
	// Price at the range bottom (lev 1): 10 levels × notional 10 = 100
	// bought at an average entry of 105 → 100 × (100−105)/105 ≈ −4.7619.
	_, unrealized, notional := neutralGridPaperPNL(lower, upper, 20, investment, 1, 0, 0, mustDecimal("100"), feeBps)
	if !unrealized.IsNegative() {
		t.Fatalf("inventory at range bottom must be under water, got %s", unrealized)
	}
	if notional.Cmp(mustDecimal("100")) != 0 {
		t.Fatalf("full bottom inventory must hold 100 notional at lev 1, got %s", notional)
	}
	// Above the midpoint the grid holds SHORT inventory: a price above the
	// short entry is under water, symmetric to the long case.
	_, unrealized, notional = neutralGridPaperPNL(lower, upper, 20, investment, 1, 10, 10, mustDecimal("118"), feeBps)
	if !unrealized.IsNegative() {
		t.Fatalf("short inventory above mid must be under water, got %s", unrealized)
	}
	if notional.Cmp(mustDecimal("80")) != 0 {
		t.Fatalf("8 levels above mid must hold 80 notional at lev 1, got %s", notional)
	}
	// The short leg earns when price falls back through the levels it sold:
	// 6 return crossings close 6 of the 8 short levels → 6 completed pairs.
	// (Residual inventory is always under water by construction — recovered
	// value is booked as realized pairs, never as positive unrealized.)
	shortReturn, _, _ := neutralGridPaperPNL(lower, upper, 20, investment, 1, 18, 12, mustDecimal("106"), feeBps)
	expectedReturn := mustDecimal("0.4855")
	if shortReturn.Sub(expectedReturn).Abs().GreaterThan(mustDecimal("0.0001")) {
		t.Fatalf("short return leg must earn 6 completed pairs ≈ 0.4855, got %s", shortReturn)
	}
	// At the midpoint itself there is no inventory in either direction.
	_, unrealized, notional = neutralGridPaperPNL(lower, upper, 20, investment, 1, 10, 10, mustDecimal("110"), feeBps)
	if !unrealized.IsZero() || !notional.IsZero() {
		t.Fatalf("no inventory at mid, got unrealized %s notional %s", unrealized, notional)
	}
}

func TestNeutralGridPaperPNLLeverageScales(t *testing.T) {
	lower, upper := mustDecimal("100"), mustDecimal("120")
	investment := mustDecimal("200")
	feeBps := mustDecimal("5")
	profit1, unrealized1, notional1 := neutralGridPaperPNL(lower, upper, 20, investment, 1, 2, 10, mustDecimal("110"), feeBps)
	profit4, unrealized4, notional4 := neutralGridPaperPNL(lower, upper, 20, investment, 4, 2, 10, mustDecimal("110"), feeBps)
	for name, pair := range map[string][2]decimal.Decimal{
		"pairProfit": {profit1, profit4},
		"unrealized": {unrealized1, unrealized4},
		"notional":   {notional1, notional4},
	} {
		if pair[1].Cmp(pair[0].Mul(decimal.NewFromInt(4))) != 0 {
			t.Fatalf("%s must scale ×4 with leverage: lev1 %s vs lev4 %s", name, pair[0], pair[1])
		}
	}
}

func TestFundingAccrual(t *testing.T) {
	now := time.Now()
	exposure := mustDecimal("400")
	rate := mustDecimal("10")
	// No full 8h boundary yet → nothing.
	if delta, _ := fundingAccrual(exposure, rate, now.Add(-7*time.Hour-time.Minute), nil, now); delta != nil {
		t.Fatalf("7h59m must not accrue, got %s", delta)
	}
	// 17h → 2 boundaries × 400 × 0.001 = 0.8, anchor advances by 16h.
	delta, next := fundingAccrual(exposure, rate, now.Add(-17*time.Hour), nil, now)
	if delta == nil || next == nil {
		t.Fatalf("17h must accrue 2 boundaries")
	}
	if delta.Cmp(mustDecimal("0.8")) != 0 {
		t.Fatalf("2 boundaries on 400 at 10bps must be 0.8, got %s", delta)
	}
	if !next.Equal(now.Add(-17 * time.Hour).Add(16 * time.Hour)) {
		t.Fatalf("anchor must advance by exactly 16h, got %v", next)
	}
	// A persisted last_funding_at newer than opened_at wins as the anchor.
	delta2, _ := fundingAccrual(exposure, rate, now.Add(-17*time.Hour), ptrTime(now.Add(-9*time.Hour)), now)
	if delta2 == nil || delta2.Cmp(mustDecimal("0.4")) != 0 {
		t.Fatalf("one boundary since the newer anchor must accrue 0.4, got %v", delta2)
	}
	// Zero exposure or zero rate → nothing to settle.
	if delta3, _ := fundingAccrual(decimal.Zero, rate, now.Add(-20*time.Hour), nil, now); delta3 != nil {
		t.Fatalf("zero exposure must not accrue, got %s", delta3)
	}
	if delta4, _ := fundingAccrual(exposure, decimal.Zero, now.Add(-20*time.Hour), nil, now); delta4 != nil {
		t.Fatalf("zero rate must not accrue, got %s", delta4)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestGridLevelForPriceClamps(t *testing.T) {
	lower, upper := mustDecimal("100"), mustDecimal("120")
	if got := gridLevelForPrice(lower, upper, 20, mustDecimal("50")); got != 0 {
		t.Fatalf("below range must clamp to 0, got %d", got)
	}
	if got := gridLevelForPrice(lower, upper, 20, mustDecimal("999")); got != 19 {
		t.Fatalf("above range must clamp to last level, got %d", got)
	}
	if got := gridLevelForPrice(lower, upper, 20, mustDecimal("110")); got != 10 {
		t.Fatalf("mid must map to level 10, got %d", got)
	}
}

func TestDeriveAISettings(t *testing.T) {
	suggested, notes := deriveAISettings([]AISample{
		{Symbol: "A", Volatility: 2, MaxDrawDown: 5},
		{Symbol: "B", Volatility: 4, MaxDrawDown: 9},
		{Symbol: "C", Volatility: 8, MaxDrawDown: 14},
		{Symbol: "D", Volatility: 12, MaxDrawDown: 22},
	})
	if suggested["pnlTargetMode"] != "DYNAMIC" || suggested["aiKitEnabled"] != true {
		t.Fatalf("mode suggestions wrong: %+v", suggested)
	}
	minVol := suggested["minVolatilityPct"].(float64)
	maxVol := suggested["maxVolatilityPct"].(float64)
	if !(minVol > 0 && maxVol > minVol) {
		t.Fatalf("volatility band invalid: %v–%v", minVol, maxVol)
	}
	if suggested["leverage"].(int) < 1 || suggested["leverage"].(int) > 3 {
		t.Fatalf("leverage out of range: %v", suggested["leverage"])
	}
	if suggested["maxDrawdownPct"].(float64) < 8 {
		t.Fatalf("drawdown floor must hold: %v", suggested["maxDrawdownPct"])
	}
	if len(notes) < 2 {
		t.Fatalf("expected explanatory notes, got %v", notes)
	}

	// Wild market steps leverage down to 1x.
	wild, _ := deriveAISettings([]AISample{
		{Symbol: "A", Volatility: 10, MaxDrawDown: 20},
		{Symbol: "B", Volatility: 14, MaxDrawDown: 26},
		{Symbol: "C", Volatility: 18, MaxDrawDown: 30},
	})
	if wild["leverage"].(int) != 1 {
		t.Fatalf("wild market must suggest 1x leverage, got %v", wild["leverage"])
	}
}

func TestClampDecimalStepBoundsMove(t *testing.T) {
	current := mustDecimal("18")
	// Proposal far below: a move of at most -30% per round.
	next := clampDecimalStep(current, mustDecimal("2"),
		mustDecimal("0.3"), mustDecimal("8"), mustDecimal("30"))
	if !next.Equal(mustDecimal("12.6")) {
		t.Fatalf("expected one bounded step to 12.6, got %s", next)
	}
	// Proposal inside the band moves directly.
	next = clampDecimalStep(current, mustDecimal("17"),
		mustDecimal("0.3"), mustDecimal("8"), mustDecimal("30"))
	if !next.Equal(mustDecimal("17")) {
		t.Fatalf("expected direct move to 17, got %s", next)
	}
	// Absolute bounds always win.
	next = clampDecimalStep(mustDecimal("9"), mustDecimal("40"),
		mustDecimal("0.3"), mustDecimal("8"), mustDecimal("30"))
	if !next.Equal(mustDecimal("11.7")) {
		t.Fatalf("expected 9+30%%=11.7 capped below 30, got %s", next)
	}
	low := clampDecimalStep(mustDecimal("8.5"), mustDecimal("1"),
		mustDecimal("0.3"), mustDecimal("8"), mustDecimal("30"))
	if !low.Equal(mustDecimal("8")) {
		t.Fatalf("expected floor 8, got %s", low)
	}
}

func TestNormalizePercent(t *testing.T) {
	cases := map[string]float64{"0.05": 5, "0.85": 85, "3": 3, "150": 150, "0": 0, "-2": 0, "600": 0}
	for input, expected := range cases {
		value, _ := strconv.ParseFloat(input, 64)
		if got := normalizePercent(value); got != expected {
			t.Fatalf("normalizePercent(%s) = %v, want %v", input, got, expected)
		}
	}
}

func TestPercentileBounds(t *testing.T) {
	values := []float64{2, 4, 8, 12}
	if p := percentile(values, 0); p != 2 {
		t.Fatalf("P0 must be min, got %v", p)
	}
	if p := percentile(values, 1); p != 12 {
		t.Fatalf("P100 must be max, got %v", p)
	}
	if p := percentile(values, 0.5); p != 8 && p != 4 {
		t.Fatalf("median must be within middle values, got %v", p)
	}
	if p := percentile(nil, 0.5); p != 0 {
		t.Fatalf("empty input must return 0, got %v", p)
	}
}
