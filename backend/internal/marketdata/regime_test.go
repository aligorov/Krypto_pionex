package marketdata

import (
	"math"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func candle(index int, close float64) pionex.KlineCandle {
	return pionex.KlineCandle{
		Time:   int64(index),
		Open:   decimal.NewFromFloat(close),
		Close:  decimal.NewFromFloat(close),
		High:   decimal.NewFromFloat(close * 1.01),
		Low:    decimal.NewFromFloat(close * 0.99),
		Volume: decimal.NewFromFloat(100),
	}
}

func synthCandles(pattern func(i int) float64, count int) []pionex.KlineCandle {
	candles := make([]pionex.KlineCandle, 0, count)
	for i := 0; i < count; i++ {
		candles = append(candles, candle(i, pattern(i)))
	}
	return candles
}

func TestDetectRegimeTrendUp(t *testing.T) {
	candles := synthCandles(func(i int) float64 { return 100 * math.Pow(1.02, float64(i)) }, 80)
	regime := DetectRegime(candles)
	if regime.Regime != "TREND_UP" {
		t.Fatalf("expected TREND_UP, got %+v", regime)
	}
	if regime.RecommendedTrend() != "long" {
		t.Fatalf("expected long grid, got %s", regime.RecommendedTrend())
	}
}

func TestDetectRegimeTrendDown(t *testing.T) {
	candles := synthCandles(func(i int) float64 { return 100 * math.Pow(0.98, float64(i)) }, 80)
	regime := DetectRegime(candles)
	if regime.Regime != "TREND_DOWN" {
		t.Fatalf("expected TREND_DOWN, got %+v", regime)
	}
	if regime.RecommendedTrend() != "short" {
		t.Fatalf("expected short grid, got %s", regime.RecommendedTrend())
	}
}

func TestDetectRegimeRange(t *testing.T) {
	candles := synthCandles(func(i int) float64 {
		return 100 + 3*math.Sin(float64(i)/4)
	}, 120)
	regime := DetectRegime(candles)
	if regime.Regime != "RANGE" {
		t.Fatalf("expected RANGE, got %+v", regime)
	}
	if regime.RecommendedTrend() != "no_trend" {
		t.Fatalf("expected neutral grid, got %s", regime.RecommendedTrend())
	}
}

func TestRangePositionBounds(t *testing.T) {
	candles := synthCandles(func(i int) float64 { return 100 + float64(i) }, 60)
	position := rangePositionPct(candles)
	if position < 95 || position > 100 {
		t.Fatalf("expected position near window top, got %f", position)
	}
}

func TestSupportResistanceRangeRespectsPrice(t *testing.T) {
	candles := synthCandles(func(i int) float64 { return 100 + 2*math.Sin(float64(i)/3) }, 80)
	lower, upper := supportResistanceRange(candles, 100, 8)
	if !(lower < 100 && 100 < upper) {
		t.Fatalf("expected price inside range, got [%f, %f]", lower, upper)
	}
	if upper >= 100*1.05 || lower <= 100*0.95 {
		t.Fatalf("expected volatility-bounded range, got [%f, %f]", lower, upper)
	}
}

func TestChoppinessIndex(t *testing.T) {
	// Synthesize a flat oscillating channel -> high Choppiness (> 55)
	rangeCandles := synthCandles(func(i int) float64 {
		return 100 + 2*math.Sin(float64(i)/2)
	}, 60)
	chopRange := ChoppinessIndex(rangeCandles, 14)
	if chopRange < 55.0 {
		t.Fatalf("expected high Choppiness in range market, got %f", chopRange)
	}

	// Synthesize an explosive linear trend -> low Choppiness (< 40)
	trendCandles := synthCandles(func(i int) float64 {
		return 100 + float64(i)*2.0
	}, 60)
	chopTrend := ChoppinessIndex(trendCandles, 14)
	if chopTrend > 40.0 {
		t.Fatalf("expected low Choppiness in strong trend, got %f", chopTrend)
	}
}

func TestBollingerBandWidthAndSqueeze(t *testing.T) {
	// Flat candles with minimal variation -> squeeze detected
	flatCandles := synthCandles(func(i int) float64 {
		return 100 + 0.05*math.Sin(float64(i))
	}, 40)
	bbw, isSqueeze := BollingerBandWidth(flatCandles, 20, 2.0)
	if bbw <= 0 {
		t.Fatalf("expected positive BBW, got %f", bbw)
	}
	if !isSqueeze {
		t.Fatalf("expected isSqueeze to be true for flat channel, got %v (bbw=%f)", isSqueeze, bbw)
	}
}
