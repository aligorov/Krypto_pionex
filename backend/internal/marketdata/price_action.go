package marketdata

import (
	"math"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// CandleMetrics represents the mathematical anatomy of an OHLCV candlestick.
type CandleMetrics struct {
	Open           float64
	High           float64
	Low            float64
	Close          float64
	Volume         float64
	Range          float64
	Body           float64
	UpperWick      float64
	LowerWick      float64
	BodyRatio      float64 // Body / Range (0..1)
	UpperWickRatio float64 // UpperWick / Range (0..1)
	LowerWickRatio float64 // LowerWick / Range (0..1)
	IsBullish      bool
	IsBearish      bool
	IsDoji         bool
	IsPinBarBull   bool // Rejection wick below (Hammer / Bullish Pin Bar)
	IsPinBarBear   bool // Rejection wick above (Shooting Star / Bearish Pin Bar)
	IsMarubozuBull bool // Strong impulsive green candle without wicks
	IsMarubozuBear bool // Strong impulsive red candle without wicks (Falling knife)
}

// AnalyzeCandle extracts the exact anatomical metrics of a single candle.
func AnalyzeCandle(c pionex.KlineCandle) CandleMetrics {
	open, _ := c.Open.Float64()
	high, _ := c.High.Float64()
	low, _ := c.Low.Float64()
	closePrice, _ := c.Close.Float64()
	volume, _ := c.Volume.Float64()

	metrics := CandleMetrics{
		Open:   open,
		High:   high,
		Low:    low,
		Close:  closePrice,
		Volume: volume,
	}

	candleRange := high - low
	if candleRange <= 0 {
		return metrics
	}
	metrics.Range = candleRange

	body := math.Abs(closePrice - open)
	metrics.Body = body
	metrics.BodyRatio = clamp(body/candleRange, 0, 1)

	upperBody := math.Max(open, closePrice)
	lowerBody := math.Min(open, closePrice)

	metrics.UpperWick = math.Max(0, high-upperBody)
	metrics.LowerWick = math.Max(0, lowerBody-low)

	metrics.UpperWickRatio = clamp(metrics.UpperWick/candleRange, 0, 1)
	metrics.LowerWickRatio = clamp(metrics.LowerWick/candleRange, 0, 1)

	metrics.IsBullish = closePrice > open
	metrics.IsBearish = closePrice < open
	metrics.IsDoji = metrics.BodyRatio <= 0.10

	// Bullish Pin Bar / Hammer: Lower wick is at least 60% of total candle range,
	// body is compressed in the top 40% of the range.
	bodyTopRatio := (upperBody - low) / candleRange
	if metrics.LowerWickRatio >= 0.60 && metrics.BodyRatio <= 0.35 && bodyTopRatio >= 0.60 {
		metrics.IsPinBarBull = true
	}

	// Bearish Pin Bar / Shooting Star: Upper wick is at least 60% of total candle range,
	// body is compressed in the bottom 40% of the range.
	bodyBottomRatio := (high - lowerBody) / candleRange
	if metrics.UpperWickRatio >= 0.60 && metrics.BodyRatio <= 0.35 && bodyBottomRatio >= 0.60 {
		metrics.IsPinBarBear = true
	}

	// Marubozu: Momentum expansion with body >= 80% and negligible wicks (< 15% each)
	if metrics.BodyRatio >= 0.80 && metrics.UpperWickRatio <= 0.15 && metrics.LowerWickRatio <= 0.15 {
		if metrics.IsBullish {
			metrics.IsMarubozuBull = true
		} else if metrics.IsBearish {
			metrics.IsMarubozuBear = true
		}
	}

	return metrics
}

// DetectEngulfing determines if curr candle completely engulfs prev candle's body with high momentum.
func DetectEngulfing(prev, curr pionex.KlineCandle) (isEngulfing bool, side string) {
	prevMetrics := AnalyzeCandle(prev)
	currMetrics := AnalyzeCandle(curr)

	if prevMetrics.Range <= 0 || currMetrics.Range <= 0 {
		return false, ""
	}

	prevOpen, _ := prev.Open.Float64()
	prevClose, _ := prev.Close.Float64()
	currOpen, _ := curr.Open.Float64()
	currClose, _ := curr.Close.Float64()

	prevUpper := math.Max(prevOpen, prevClose)
	prevLower := math.Min(prevOpen, prevClose)
	currUpper := math.Max(currOpen, currClose)
	currLower := math.Min(currOpen, currClose)

	// Bullish Engulfing: previous was red, current is green and engulfs previous body
	if prevMetrics.IsBearish && currMetrics.IsBullish {
		if currLower <= prevLower && currUpper >= prevUpper && currMetrics.BodyRatio >= 0.60 {
			return true, "BULLISH"
		}
	}

	// Bearish Engulfing: previous was green, current is red and engulfs previous body
	if prevMetrics.IsBullish && currMetrics.IsBearish {
		if currUpper >= prevUpper && currLower <= prevLower && currMetrics.BodyRatio >= 0.60 {
			return true, "BEARISH"
		}
	}

	return false, ""
}

// DetectSFP (Swing Failure Pattern) detects liquidity sweeps:
// Price probes below key support (priorLow), triggers stops, but closes back ABOVE it with a rejection wick.
func DetectSFP(priorLow decimal.Decimal, curr pionex.KlineCandle) (isSweep bool, wickRatio float64) {
	if priorLow.LessThanOrEqual(decimal.Zero) {
		return false, 0
	}
	metrics := AnalyzeCandle(curr)
	if metrics.Range <= 0 {
		return false, 0
	}

	priorLowF, _ := priorLow.Float64()

	// Condition 1: Low broke below the prior level (sweep)
	sweptBelow := metrics.Low < priorLowF

	// Condition 2: Close held or reclaimed above the prior level (acceptance back inside)
	closedAbove := metrics.Close >= priorLowF

	// Condition 3: Meaningful rejection wick
	hasWick := metrics.LowerWickRatio >= 0.50

	if sweptBelow && closedAbove && hasWick {
		return true, metrics.LowerWickRatio
	}
	return false, metrics.LowerWickRatio
}
