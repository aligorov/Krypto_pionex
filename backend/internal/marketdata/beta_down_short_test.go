package marketdata

import "testing"

// TestAntiFomoShortFloorsLifted pins the v2.0.147 beta-down exemption
// semantics: floors lift only on the cascade pass or when the pair's own
// tape confirms the downtrend (strong trend + falling EMA). A rising pair
// in a beta-down scan is a divergence and keeps the floors armed.
func TestAntiFomoShortFloorsLifted(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cascade, betaDown bool
		adx, slope        float64
		wantLifted        bool
	}{
		{"cascade window always lifts", true, false, 15, -0.2, true},
		{"beta down confirmed downtrend by ADX", false, true, 29, -1.4, true},
		{"beta down confirmed by steep slope", false, true, 15, -0.9, true},
		{"beta down flat pair stays armed", false, true, 15, -0.2, false},
		{"beta down rising pair stays armed", false, true, 15, 1.2, false},
		{"beta down strong ADX but rising stays armed", false, true, 29, 0.8, false},
		{"no flags stays armed", false, false, 29, -1.4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := antiFomoShortFloorsLifted(tc.cascade, tc.betaDown, tc.adx, tc.slope); got != tc.wantLifted {
				t.Fatalf("antiFomoShortFloorsLifted(%v, %v, %.1f, %.1f) = %v, want %v",
					tc.cascade, tc.betaDown, tc.adx, tc.slope, got, tc.wantLifted)
			}
		})
	}
}
