package marketdata

import (
	"math"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func candleAt(low, high, volume float64) pionex.KlineCandle {
	return pionex.KlineCandle{
		Open:   decimal.NewFromFloat(low),
		Close:  decimal.NewFromFloat((low + high) / 2),
		Low:    decimal.NewFromFloat(low),
		High:   decimal.NewFromFloat(high),
		Volume: decimal.NewFromFloat(volume),
	}
}

// A quiet tail after a wild history must rank LOW: bands are tighter than
// most of the window. The reversed shape must rank HIGH.
func TestBBWPercentileRank(t *testing.T) {
	wild := make([]pionex.KlineCandle, 0, 120)
	for i := 0; i < 100; i++ {
		base := 100.0 + 2.5*math.Sin(float64(i)/3.0) // ±2.5% swings
		wild = append(wild, candleAt(base*0.985, base*1.015, 10))
	}
	quietTail := make([]pionex.KlineCandle, len(wild))
	copy(quietTail, wild)
	for i := 0; i < 20; i++ {
		base := 100.0 + 0.15*math.Sin(float64(i)/2.0) // ±0.15% squeeze
		quietTail = append(quietTail, candleAt(base*0.998, base*1.002, 10))
	}
	low := DetectRegime(quietTail).BBWPercentile
	if low >= 35 {
		t.Fatalf("squeeze tail must rank low, got %.1f", low)
	}

	quietHistory := make([]pionex.KlineCandle, 0, 120)
	for i := 0; i < 100; i++ {
		base := 100.0 + 0.15*math.Sin(float64(i)/2.0)
		quietHistory = append(quietHistory, candleAt(base*0.998, base*1.002, 10))
	}
	wildTail := make([]pionex.KlineCandle, len(quietHistory))
	copy(wildTail, quietHistory)
	for i := 0; i < 20; i++ {
		base := 100.0 + 2.5*math.Sin(float64(i)/3.0)
		wildTail = append(wildTail, candleAt(base*0.985, base*1.015, 10))
	}
	high := DetectRegime(wildTail).BBWPercentile
	if high <= 60 {
		t.Fatalf("expansion tail must rank high, got %.1f", high)
	}
}

// The volume band must sit on the dominant cluster, not the outlier wicks.
func TestVolumeProfileBoundsAnchorsOnDominantCluster(t *testing.T) {
	candles := make([]pionex.KlineCandle, 0, 80)
	for i := 0; i < 60; i++ {
		candles = append(candles, candleAt(100, 110, 10)) // dominant node
	}
	for i := 0; i < 20; i++ {
		candles = append(candles, candleAt(130, 140, 1)) // thin outlier zone
	}
	lower, upper, ok := volumeProfileBounds(candles, 0.7)
	if !ok {
		t.Fatalf("expected bounds to be computable")
	}
	if lower < 99 || upper > 112 {
		t.Fatalf("band must anchor on the 100-110 node, got [%.2f, %.2f]", lower, upper)
	}
}

// A degenerate window (no volume info) must report not-ok, never bounds.
func TestVolumeProfileBoundsDegenerate(t *testing.T) {
	if _, _, ok := volumeProfileBounds([]pionex.KlineCandle{}, 0.7); ok {
		t.Fatalf("empty window must not produce bounds")
	}
}

// The volume anchor must never break the price bracket or squeeze the range
// below the minimum span — the structural fallback stands in that case.
func TestSupportResistanceRangeVolumeAnchored(t *testing.T) {
	candles := make([]pionex.KlineCandle, 0, 80)
	for i := 0; i < 60; i++ {
		candles = append(candles, candleAt(100, 110, 10))
	}
	for i := 0; i < 20; i++ {
		candles = append(candles, candleAt(118, 122, 3)) // price trades here
	}
	price := 120.0
	lower, upper := supportResistanceRange(candles, price, 3.0)
	if !(lower < price && price < upper) {
		t.Fatalf("bounds must bracket price, got [%.2f, %.2f]", lower, upper)
	}
	if upper-lower < price*0.02 {
		t.Fatalf("span must stay >= 2%% of price, got %.2f", upper-lower)
	}
	if upper > 123.5 {
		t.Fatalf("wick zone above the volume band must be trimmed, got upper %.2f", upper)
	}
}
