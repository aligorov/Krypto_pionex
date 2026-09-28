package autogrid

import "testing"

// betaDownExemptFixture is the counterfactual's money shape (2026-09-28):
// a scanner-short candidate pinned at the channel BOTTOM — the zone both
// entry-timing and the anti-FOMO floors reject — inside a confirmed
// downtrend (ONDO-class: ADX ~29, EMA slope ~−1.4%, position ~15%).
func betaDownExemptFixture(trend string, adx, slope, rangePos float64) Candidate {
	return Candidate{
		Symbol:           "ONDO_USDT_PERP",
		RecommendedTrend: trend,
		ModelAssumptions: map[string]any{
			"adx":              adx,
			"emaSlopePct":      slope,
			"rangePositionPct": rangePos,
			"confluence": map[string]any{
				"fibInGoldenPocket": false,
				"macdCrossedDown":   false,
				"stochCrossedDown":  false,
				"srNearestSupport":  0.0,
			},
		},
	}
}

// TestBetaDownShortExempt pins the exemption predicate itself: only a
// scanner-short on a strong falling tape during a live BTC TREND_DOWN.
func TestBetaDownShortExempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trend    string
		adx      float64
		slope    float64
		betaDown bool
		want     bool
	}{
		{"confirmed downtrend short", "short", 29, -1.4, true, true},
		{"steep slope short", "short", 15, -0.9, true, true},
		{"flat pair not exempt", "short", 15, -0.2, true, false},
		{"rising pair not exempt", "short", 15, 1.2, true, false},
		{"strong ADX rising not exempt", "short", 29, 0.8, true, false},
		{"neutral never exempt", "no_trend", 29, -1.4, true, false},
		{"long never exempt", "long", 29, -1.4, true, false},
		{"no beta down not exempt", "short", 29, -1.4, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := betaDownExemptFixture(tc.trend, tc.adx, tc.slope, 15)
			if got := betaDownShortExempt(c, tc.betaDown); got != tc.want {
				t.Fatalf("betaDownShortExempt(%s, adx=%.1f, slope=%.1f, betaDown=%v) = %v, want %v",
					tc.trend, tc.adx, tc.slope, tc.betaDown, got, tc.want)
			}
		})
	}
}

// TestBetaDownShortExemptComposition pins the deploy-loop wiring: the gate
// expression `!isEntryTimingFavorable(c) && !betaDownShortExempt(c, down)`
// must pass the counterfactual shape ONLY while beta is down — the
// incumbent rejection stays byte-identical in every other regime.
func TestBetaDownShortExemptComposition(t *testing.T) {
	c := betaDownExemptFixture("short", 29, -1.4, 15)
	if isEntryTimingFavorable(c) {
		t.Fatal("fixture must sit OUTSIDE the short entry-timing zone (position 15% < 28% floor)")
	}
	if !betaDownShortExempt(c, true) {
		t.Fatal("fixture must be exempt in a beta-down regime")
	}
	if !isEntryTimingFavorable(c) && !betaDownShortExempt(c, true) {
		t.Fatal("beta-down composition must ADMIT the confirmed-downtrend short")
	}
	if !isEntryTimingFavorable(c) && !betaDownShortExempt(c, false) {
		// correct: incumbent behavior — rejected outside a beta-down regime
	} else {
		t.Fatal("non-beta-down composition must still REJECT the same candidate")
	}
}
