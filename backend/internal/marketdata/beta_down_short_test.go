package marketdata

import "testing"

// TestAntiFomoShortFloorsLifted pins the v2.0.162 semantics: floors lift
// on the cascade pass OR whenever the pair's own tape confirms the
// downtrend (strong trend + falling EMA) — the beta-down flag is subsumed
// by the own-tape confirmation. A rising or flat pair keeps the floors
// armed in every mode.
func TestAntiFomoShortFloorsLifted(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cascade, betaDown bool
		adx, slope        float64
		wantLifted        bool
	}{
		{"cascade window always lifts", true, false, 15, -0.2, true},
		{"confirmed downtrend by ADX (no flags)", false, false, 29, -1.4, true},
		{"confirmed by steep slope (no flags)", false, false, 15, -0.9, true},
		{"beta down confirmed downtrend", false, true, 29, -1.4, true},
		{"flat pair stays armed", false, true, 15, -0.2, false},
		{"rising pair stays armed", false, true, 15, 1.2, false},
		{"strong ADX but rising stays armed", false, true, 29, 0.8, false},
		{"weak flat tape stays armed", false, false, 15, -0.2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := antiFomoShortFloorsLifted(tc.cascade, tc.betaDown, tc.adx, tc.slope); got != tc.wantLifted {
				t.Fatalf("antiFomoShortFloorsLifted(%v, %v, %.1f, %.1f) = %v, want %v",
					tc.cascade, tc.betaDown, tc.adx, tc.slope, got, tc.wantLifted)
			}
		})
	}
}
