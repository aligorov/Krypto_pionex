package autogrid

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

// stressCase is the shared audit-case geometry (v2.0.139 package C1): a
// $100×2x NEUTRAL grid on a 95..105 span, entry exactly at mid, 20 levels
// (step 0.5, levels 95.0 .. 104.5), anti-hunt stop 93.5 (1.5 below lower).
func stressCaseGeometry() (entry, lower, upper, stop decimal.Decimal) {
	return decimal.NewFromInt(100),
		decimal.NewFromInt(95),
		decimal.NewFromInt(105),
		decimal.NewFromFloat(93.5)
}

// TestStressInventoryLossNeutralHandComputed pins the review-corrected
// NEUTRAL downside model: each downside level's MTM ALREADY carries its unit
// to the stop (including the segment below the lower bound) and there is
// deliberately NO carry leg — the first draft double-counted that segment
// and implied a $300 position on a $200-margin slot. Expected pieces at the
// audit geometry:
//
//	10 filled downside levels (95.0..99.5, $10 each)  → 3.835066
//	exit cost 0.10% on the $100 held at the stop      → 0.100000
//	total                                              → 3.935066
//
// The level count also agrees with neutralGridPaperPNL's half-clamp: entry
// at mid loads exactly the grid's lower half — half the notional at the
// bound, never the full leveraged position.
func TestStressInventoryLossNeutralHandComputed(t *testing.T) {
	entry, lower, upper, stop := stressCaseGeometry()
	got := stressInventoryLossUSDT("neutral", entry, lower, upper, stop, 20,
		decimal.NewFromInt(100), 2)
	if !got.IsPositive() {
		t.Fatalf("stress loss must be positive, got %s", got)
	}
	if diff := math.Abs(got.InexactFloat64() - 3.935066); diff > 0.005 {
		t.Fatalf("hand-computed NEUTRAL stress loss %s, want 3.935066 ±0.005", got.StringFixed(6))
	}
	// Independent base-units formulation: base_i = notional_i/p_i bought at
	// p_i, losing base_i×(p_i−stop) on the ride to the stop — rebuilt from
	// raw differences instead of ratios, over the filled downside levels only.
	per := decimal.NewFromInt(10)
	want := decimal.Zero
	for i := 0; i < 10; i++ {
		p := decimal.NewFromInt(95).Add(decimal.NewFromFloat(float64(i)).Mul(decimal.NewFromFloat(0.5)))
		base := per.Div(p)
		want = want.Add(base.Mul(p.Sub(stop)))
	}
	want = want.Add(decimal.NewFromInt(100).Mul(decimal.NewFromFloat(0.001)))
	if diff := math.Abs(got.Sub(want).InexactFloat64()); diff > 0.000001 {
		t.Fatalf("base-units cross-check: got %s, independent %s", got.StringFixed(8), want.StringFixed(8))
	}
	// Direction casing must not change the number.
	if again := stressInventoryLossUSDT("NEUTRAL", entry, lower, upper, stop, 20, decimal.NewFromInt(100), 2); !again.Equal(got) {
		t.Fatalf("NEUTRAL casing diverged: %s vs %s", again, got)
	}
}

// TestStressInventoryLossDirectionalFullNotional pins the v2.0.97
// exchange-truth semantics (paperInitialInventoryNotional): a directional
// grid opens the FULL leveraged notional at entry, so the adverse traverse
// is notionalFull×(1 − stop/entry) + 0.10% exit on the full notional — no
// per-level additions on top of an already-full position. $200×2x from 100
// to a 93.5 stop: 400×0.065 = 26.00 + 0.40 = 26.40 (the first draft's
// levels+carry(bound) shape understated this by ~45% — review P1-2).
func TestStressInventoryLossDirectionalFullNotional(t *testing.T) {
	entry, lower, upper, stop := stressCaseGeometry()
	got := stressInventoryLossUSDT("long", entry, lower, upper, stop, 20,
		decimal.NewFromInt(200), 2)
	if diff := math.Abs(got.InexactFloat64() - 26.40); diff > 0.005 {
		t.Fatalf("hand-computed LONG stress loss %s, want 26.40 ±0.005", got.StringFixed(6))
	}
	if again := stressInventoryLossUSDT("LONG", entry, lower, upper, stop, 20, decimal.NewFromInt(200), 2); !again.Equal(got) {
		t.Fatalf("LONG casing diverged: %s vs %s", again, got)
	}
	// SHORT mirror: full notional from entry up to a stop ABOVE entry.
	shortStop := decimal.NewFromFloat(106.5)
	shortGot := stressInventoryLossUSDT("short", entry, lower, upper, shortStop, 20,
		decimal.NewFromInt(200), 2)
	if diff := math.Abs(shortGot.InexactFloat64() - 26.40); diff > 0.005 {
		t.Fatalf("hand-computed SHORT stress loss %s, want 26.40 ±0.005", shortGot.StringFixed(6))
	}
	// Degenerate side: a long's stop above entry must not produce a loss.
	if bad := stressInventoryLossUSDT("long", entry, lower, upper, shortStop, 20, decimal.NewFromInt(200), 2); !bad.IsZero() {
		t.Fatalf("LONG with stop above entry must be zero, got %s", bad)
	}
}

// TestStressInventoryLossStopInsideRange: a stop at/inside the range has
// only the in-range levels at/above the stop fill before the bot dies.
// Stop 96: levels 96.0..99.5 (8 × $10 → 1.421430), exit 0.10% on $80
// (→ 0.08) = 1.501430 total.
func TestStressInventoryLossStopInsideRange(t *testing.T) {
	entry, lower, upper, _ := stressCaseGeometry()
	stop := decimal.NewFromInt(96)
	got := stressInventoryLossUSDT("neutral", entry, lower, upper, stop, 20,
		decimal.NewFromInt(100), 2)
	if diff := math.Abs(got.InexactFloat64() - 1.501430); diff > 0.005 {
		t.Fatalf("in-range stop stress loss %s, want 1.501430 ±0.005", got.StringFixed(6))
	}
}

// TestStressInventoryLossDegenerateGuards: zero gridNum / investment /
// leverage, inverted or non-positive prices all yield zero — the caller
// treats that as "no floor".
func TestStressInventoryLossDegenerateGuards(t *testing.T) {
	entry, lower, upper, stop := stressCaseGeometry()
	invest := decimal.NewFromInt(100)
	cases := []struct {
		name                      string
		direction                 string
		entry, lower, upper, stop decimal.Decimal
		gridNum, lev              int
		invest                    decimal.Decimal
	}{
		{"zero gridNum", "neutral", entry, lower, upper, stop, 0, 2, invest},
		{"negative gridNum", "neutral", entry, lower, upper, stop, -3, 2, invest},
		{"zero invest", "neutral", entry, lower, upper, stop, 20, 2, decimal.Zero},
		{"zero leverage", "neutral", entry, lower, upper, stop, 20, 0, invest},
		{"inverted bounds", "neutral", entry, upper, lower, stop, 20, 2, invest},
		{"zero stop", "neutral", entry, lower, upper, decimal.Zero, 20, 2, invest},
		{"zero lower", "neutral", entry, decimal.Zero, upper, stop, 20, 2, invest},
	}
	for _, tc := range cases {
		if got := stressInventoryLossUSDT(tc.direction, tc.entry, tc.lower, tc.upper, tc.stop, tc.gridNum, tc.invest, tc.lev); !got.IsZero() {
			t.Fatalf("%s: want zero stress loss, got %s", tc.name, got)
		}
	}
}

// TestComputeBotTargetsStressFloor: the v2.0.139 floor — a σ-quiet pair
// derives the DynamicLossMinPct cap ($4 on $100×2x) while a LONG grid whose
// anti-hunt stop sits 6.5% below entry loses $13.20 on the full traverse
// (the true NEAR #1401 class — directional full-notional traverse; the
// corrected NEUTRAL model no longer trips this case, which is exactly why
// the first draft's inflated carry leg had to go). The stored maxLoss must
// cover the stress loss, the floored flag must mark the telemetry, and the
// 4-arg compatibility wrapper (shadow portfolio — no geometry in hand)
// must stay unfloored.
func TestComputeBotTargetsStressFloor(t *testing.T) {
	entry, lower, upper, stop := stressCaseGeometry()
	settings := Settings{
		PnLTargetMode: "DYNAMIC",
		BudgetUSDT:    decimal.NewFromInt(100),
	}
	candidate := Candidate{
		VolatilityPct:  decimal.NewFromInt(1),
		MaxDrawdownPct: decimal.NewFromInt(1),
	}
	geo := stressGeometry{
		direction: "long", entry: entry, lower: lower, upper: upper,
		stop: stop, gridNum: 20, invest: decimal.NewFromInt(100),
	}
	target, maxLoss, stress := computeBotTargetsWithStress(settings, candidate, 2, 10, geo)
	if maxLoss == nil || target == nil {
		t.Fatalf("DYNAMIC targets must be derived, got nil target/loss")
	}
	if !stress.floored {
		t.Fatalf("directional tight-stop geometry must floor the dynamic cap (stress %s vs derived 4.00)", stress.loss.StringFixed(2))
	}
	// The floor stores the 2-dp rounded stress loss — within a cent of the
	// simulated traverse, strictly above the unfloored $4.00 derivation.
	if !maxLoss.Equal(stress.loss.Round(2)) {
		t.Fatalf("floored maxLoss %s must equal the rounded stress loss %s", maxLoss, stress.loss.Round(2))
	}
	if diff := math.Abs(maxLoss.Sub(stress.loss).InexactFloat64()); diff > 0.01 {
		t.Fatalf("floored maxLoss %s drifted from stress loss %s", maxLoss, stress.loss)
	}
	if !maxLoss.GreaterThan(decimal.NewFromInt(4)) {
		t.Fatalf("floored maxLoss %s must exceed the unfloored $4.00 derivation", maxLoss)
	}
	if diff := math.Abs(stress.loss.InexactFloat64() - 13.20); diff > 0.005 {
		t.Fatalf("directional stress loss %s, want 13.20 ±0.005", stress.loss.StringFixed(6))
	}
	// Compatibility wrapper: identical inputs, no geometry → the pre-v2.0.139
	// DynamicLossMinPct derivation verbatim.
	plainTarget, plainLoss := computeBotTargets(settings, candidate, 2, 10)
	if plainTarget == nil || plainLoss == nil {
		t.Fatalf("wrapper must derive both target and loss")
	}
	if diff := math.Abs(plainLoss.InexactFloat64() - 4.0); diff > 0.000001 {
		t.Fatalf("wrapper maxLoss must stay the unfloored $4.00 derivation, got %s", plainLoss)
	}
	if diff := math.Abs(plainTarget.InexactFloat64() - 9.0); diff > 0.000001 {
		t.Fatalf("wrapper target must stay $9.00, got %s", plainTarget)
	}
}
