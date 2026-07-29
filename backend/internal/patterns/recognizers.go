package patterns

import (
	"github.com/shopspring/decimal"
)

// Candle represents deterministic OHLCV input for pattern detection.
type Candle struct {
	OpenTime  int64
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	Volume    decimal.Decimal
	CloseTime int64
}

// PatternSignal represents a detected trade opportunity.
type PatternSignal struct {
	PatternType string          // BOS, CHOCH, FVG, ORDER_BLOCK, ENGULFING, PIN_BAR
	Side        string          // LONG, SHORT
	Price       decimal.Decimal
	StopLoss    decimal.Decimal
	TakeProfit  decimal.Decimal
	Quality     float64
}

// DetectEngulfing checks for Bullish or Bearish Engulfing pattern on closed candles.
func DetectEngulfing(prev, curr Candle) *PatternSignal {
	// Bullish Engulfing
	if prev.Close.LessThan(prev.Open) && curr.Close.GreaterThan(curr.Open) {
		if curr.Open.LessThanOrEqual(prev.Close) && curr.Close.GreaterThanOrEqual(prev.Open) {
			sl := curr.Low
			tp := curr.Close.Add(curr.Close.Sub(sl).Mul(decimal.NewFromInt(2)))
			return &PatternSignal{
				PatternType: "ENGULFING",
				Side:        "LONG",
				Price:       curr.Close,
				StopLoss:    sl,
				TakeProfit:  tp,
				Quality:     0.85,
			}
		}
	}

	// Bearish Engulfing
	if prev.Close.GreaterThan(prev.Open) && curr.Close.LessThan(curr.Open) {
		if curr.Open.GreaterThanOrEqual(prev.Close) && curr.Close.LessThanOrEqual(prev.Open) {
			sl := curr.High
			tp := curr.Close.Sub(sl.Sub(curr.Close).Mul(decimal.NewFromInt(2)))
			return &PatternSignal{
				PatternType: "ENGULFING",
				Side:        "SHORT",
				Price:       curr.Close,
				StopLoss:    sl,
				TakeProfit:  tp,
				Quality:     0.85,
			}
		}
	}

	return nil
}

// DetectFVG identifies Fair Value Gaps across 3 consecutive closed candles.
func DetectFVG(c1, c2, c3 Candle) *PatternSignal {
	// Bullish FVG: Low of candle 3 is higher than High of candle 1
	if c3.Low.GreaterThan(c1.High) {
		fvgGap := c3.Low.Sub(c1.High)
		if fvgGap.GreaterThan(c2.Open.Mul(decimal.NewFromFloat(0.002))) { // min gap size
			sl := c1.Low
			tp := c3.Close.Add(c3.Close.Sub(sl).Mul(decimal.NewFromInt(2)))
			return &PatternSignal{
				PatternType: "FVG",
				Side:        "LONG",
				Price:       c3.Close,
				StopLoss:    sl,
				TakeProfit:  tp,
				Quality:     0.90,
			}
		}
	}

	// Bearish FVG: High of candle 3 is lower than Low of candle 1
	if c3.High.LessThan(c1.Low) {
		fvgGap := c1.Low.Sub(c3.High)
		if fvgGap.GreaterThan(c2.Open.Mul(decimal.NewFromFloat(0.002))) {
			sl := c1.High
			tp := c3.Close.Sub(sl.Sub(c3.Close).Mul(decimal.NewFromInt(2)))
			return &PatternSignal{
				PatternType: "FVG",
				Side:        "SHORT",
				Price:       c3.Close,
				StopLoss:    sl,
				TakeProfit:  tp,
				Quality:     0.90,
			}
		}
	}

	return nil
}
