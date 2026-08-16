package marketdata

import (
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// Series is a one-pass float64 extraction of a candle slice. Indicators used
// to re-parse decimals independently; a single extraction removes that cost
// and gives the indicator bundle a shared input.
type Series struct {
	Time   []int64
	Open   []float64
	High   []float64
	Low    []float64
	Close  []float64
	Volume []float64
}

// ExtractSeries parses candles into float64 slices, dropping non-positive
// rows so indicator math never sees zeros from malformed feeds.
func ExtractSeries(candles []pionex.KlineCandle) *Series {
	series := &Series{
		Time:   make([]int64, 0, len(candles)),
		Open:   make([]float64, 0, len(candles)),
		High:   make([]float64, 0, len(candles)),
		Low:    make([]float64, 0, len(candles)),
		Close:  make([]float64, 0, len(candles)),
		Volume: make([]float64, 0, len(candles)),
	}
	for _, candle := range candles {
		open, _ := candle.Open.Float64()
		high, _ := candle.High.Float64()
		low, _ := candle.Low.Float64()
		close_, _ := candle.Close.Float64()
		volume, _ := candle.Volume.Float64()
		if open <= 0 || high <= 0 || low <= 0 || close_ <= 0 {
			continue
		}
		series.Time = append(series.Time, candle.Time)
		series.Open = append(series.Open, open)
		series.High = append(series.High, high)
		series.Low = append(series.Low, low)
		series.Close = append(series.Close, close_)
		series.Volume = append(series.Volume, volume)
	}
	return series
}

func (s *Series) Len() int { return len(s.Close) }
