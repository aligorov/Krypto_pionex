package marketdata

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// TestStackedEMAPreservation pins the v2.0.162 regime rule: a decisively
// stacked, sloping tape must not be flattened back into RANGE by the
// oscillation/ADX overrides — the ±0.5% confirmed-trend band the
// directional engine keys on keeps its TREND_* class.
func TestStackedEMAPreservation(t *testing.T) {
	makeCandles := func(start, driftPct float64, n int) []pionex.KlineCandle {
		candles := make([]pionex.KlineCandle, 0, n)
		price := start
		for i := 0; i < n; i++ {
			price *= 1 + driftPct/100.0
			p := decimal.NewFromFloat(price)
			candles = append(candles, pionex.KlineCandle{
				Time:  int64(i) * 900_000,
				Open:  p,
				High:  p.Mul(decimal.NewFromFloat(1.010)),
				Low:   p.Mul(decimal.NewFromFloat(0.995)),
				Close: p,
			})
		}
		return candles
	}
	// Steady decline: with the stacked-EMA rule it must stay TREND_DOWN
	// even when the oscillation flattener would call it chop (pre-162 this
	// exact shape became RANGE → blind NEUTRAL grids).
	res := DetectRegime(makeCandles(100, -0.25, 60))
	if res.Regime != "TREND_DOWN" {
		t.Fatalf("stacked bearish tape (slope %.2f%%) flattened to %s", res.EMASlopePct, res.Regime)
	}
	// Steady incline: must stay TREND_UP through the flatteners.
	res = DetectRegime(makeCandles(100, 0.25, 60))
	if res.Regime != "TREND_UP" {
		t.Fatalf("stacked bullish tape (slope %.2f%%) flattened to %s", res.EMASlopePct, res.Regime)
	}
	// A pure flat tape keeps RANGE.
	res = DetectRegime(makeCandles(100, 0.0, 60))
	if res.Regime != "RANGE" {
		t.Fatalf("flat tape must stay RANGE, got %s", res.Regime)
	}
	// A FAST oscillation crosses the midline many times per window — the
	// crossings veto keeps it RANGE even though each leg stacks the EMAs
	// decisively (review P2: pin the discriminator, not just one fixture).
	sine := make([]pionex.KlineCandle, 0, 120)
	for i := 0; i < 120; i++ {
		p := decimal.NewFromFloat(100 + 3*math.Sin(float64(i)/1.5))
		sine = append(sine, pionex.KlineCandle{
			Time: int64(i) * 900_000, Open: p,
			High: p.Mul(decimal.NewFromFloat(1.010)),
			Low:  p.Mul(decimal.NewFromFloat(0.990)),
			Close: p,
		})
	}
	if res = DetectRegime(sine); res.Regime != "RANGE" {
		t.Fatalf("fast oscillation must stay RANGE (crossings veto), got %s", res.Regime)
	}
}
