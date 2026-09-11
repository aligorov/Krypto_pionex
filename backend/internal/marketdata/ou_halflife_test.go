package marketdata

import (
	"math"
	"testing"
)

// v2.0.94 entry-gate math: the shared AR(1) half-life must order regimes —
// a slow-decaying mean reversion reads a LONG half-life, a fast alternation
// reads a SHORT one, and a pure trend must not fit at all (ok=false — the
// gate skips those, Hurst/ADX vetoes own them).
func TestOUHalfLifeStepsOrdersRegimes(t *testing.T) {
	// Slow sine (period ~40 steps, amplitude 1% around 100): persistent
	// mean reversion — a long half-life in step units.
	slow := make([]float64, 200)
	for i := range slow {
		slow[i] = 100 * (1 + 0.01*math.Sin(2*math.Pi*float64(i)/40))
	}
	slowHL, ok := OUHalfLifeSteps(slow)
	if !ok {
		t.Fatal("slow sine must fit an OU process")
	}

	// Fast alternation (period 8 steps — a period-4 sine sits OUTSIDE the OU
	// class: b = 2(cos ω − 1) = −2, the divergent guard rejects it): the
	// half-life must be dramatically shorter than the slow sine's.
	fast := make([]float64, 200)
	for i := range fast {
		fast[i] = 100 * (1 + 0.01*math.Sin(2*math.Pi*float64(i)/8))
	}
	fastHL, ok := OUHalfLifeSteps(fast)
	if !ok {
		t.Fatal("fast alternation must fit an OU process")
	}
	if fastHL >= slowHL {
		t.Fatalf("fast alternation must have the shorter half-life: fast=%.2f slow=%.2f", fastHL, slowHL)
	}
	// The divergent oscillator (period 4, b ≤ −1) must NOT fit — it is not
	// an OU process and the entry gate must skip it.
	divergent := make([]float64, 200)
	for i := range divergent {
		divergent[i] = 100 * (1 + 0.01*math.Sin(2*math.Pi*float64(i)/4))
	}
	if _, ok := OUHalfLifeSteps(divergent); ok {
		t.Fatal("a period-4 oscillator is outside the OU class (b ≤ −1) and must not fit")
	}

	// Pure trend: b ≥ 0 → not mean-reverting → ok=false (gate skips).
	trend := make([]float64, 200)
	for i := range trend {
		trend[i] = 100 * (1 + 0.001*float64(i))
	}
	if _, ok := OUHalfLifeSteps(trend); ok {
		t.Fatal("a pure trend must not fit an OU process")
	}
}

// The scanner gate bar itself: MinOUHalfLifeHours must stay at the mined
// value — 2h is where the weekly ledger flips from −$0.07/bot to +$0.46.
func TestMinOUHalfLifeHoursMatchesMining(t *testing.T) {
	if MinOUHalfLifeHours != 2.0 {
		t.Fatalf("MinOUHalfLifeHours must stay 2.0 (mined flip point), got %v", MinOUHalfLifeHours)
	}
}

// CandleIntervalHours: the interval-to-hours mapping the gate multiplies the
// step-unit half-life by.
func TestCandleIntervalHours(t *testing.T) {
	cases := map[string]float64{
		"15M": 0.25, "1H": 1, "4H": 4, "1D": 24, "garbage": 0.25,
	}
	for interval, want := range cases {
		if got := CandleIntervalHours(interval); math.Abs(got-want) > 1e-9 {
			t.Fatalf("CandleIntervalHours(%q) = %v, want %v", interval, got, want)
		}
	}
}
