package marketdata

import (
	"math"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// Regime classification drives grid direction per documented best practice:
// grids harvest oscillation, so RANGE markets get neutral grids while
// confirmed trends get one-directional grids in the trend direction.
type RegimeResult struct {
	Regime              string  `json:"regime"`              // RANGE, TREND_UP, TREND_DOWN
	ADX                 float64 `json:"adx"`                 // Wilder ADX(14), trend strength
	RSI                 float64 `json:"rsi"`                 // Wilder RSI(14), momentum oscillator 0..100
	Choppiness          float64 `json:"choppiness"`          // Dreiss Choppiness Index (14), 0..100 (>61.8 range, <38.2 trend)
	BBWPct              float64 `json:"bbwPct"`              // Bollinger Band Width %
	IsSqueeze           bool    `json:"isSqueeze"`           // Volatility squeeze detected (breakout risk)
	EMAFast             float64 `json:"emaFast"`             // EMA(20) of closes
	EMASlow             float64 `json:"emaSlow"`             // EMA(50) of closes
	EMASlopePct         float64 `json:"emaSlopePct"`         // EMA20 slope over ~10 candles, %
	RangePositionPct    float64 `json:"rangePositionPct"`    // 0 = at window low, 100 = at window high
	ATRPct              float64 `json:"atrPct"`              // ATR(14) / price * 100
	ParkinsonVolatility float64 `json:"parkinsonVolatility"` // Parkinson intra-candle volatility %
}

const (
	adxTrendThreshold  = 22.0
	chopRangeThreshold = 58.0
	chopTrendThreshold = 38.2
	emaFastPeriod      = 20
	emaSlowPeriod      = 50
	adxPeriod          = 14
	bbPeriod           = 20
)

// DetectRegime computes trend/range classification from OHLCV candles.
func DetectRegime(candles []pionex.KlineCandle) RegimeResult {
	result := RegimeResult{Regime: "RANGE", Choppiness: 50.0, RSI: 50.0}
	if len(candles) < 30 {
		return result
	}
	closes := make([]float64, 0, len(candles))
	for _, candle := range candles {
		value, _ := candle.Close.Float64()
		if value > 0 {
			closes = append(closes, value)
		}
	}
	if len(closes) < 30 {
		return result
	}
	result.EMAFast = ema(closes, emaFastPeriod)
	result.EMASlow = ema(closes, emaSlowPeriod)
	result.ADX = adx(candles, adxPeriod)
	result.RSI = CalculateRSI(candles, 14)
	result.Choppiness = ChoppinessIndex(candles, adxPeriod)
	result.BBWPct, result.IsSqueeze = BollingerBandWidth(candles, bbPeriod, 2.0)
	result.ATRPct = atrPercent(candles, adxPeriod)
	result.RangePositionPct = rangePositionPct(candles)

	slopeWindow := 10
	if slopeWindow > len(closes) {
		slopeWindow = len(closes)
	}
	reference := ema(closes[:len(closes)-slopeWindow+1], emaFastPeriod)
	if reference > 0 {
		result.EMASlopePct = (result.EMAFast - reference) / reference * 100
	}
	startClose, _ := candles[0].Close.Float64()
	endClose := closes[len(closes)-1]
	windowReturnPct := 0.0
	if startClose > 0 && endClose > 0 {
		windowReturnPct = (endClose - startClose) / startClose * 100
	}

	switch {
	case result.EMAFast > result.EMASlow && result.EMASlopePct > 0.15:
		result.Regime = "TREND_UP"
	case result.EMAFast < result.EMASlow && result.EMASlopePct < -0.15:
		if windowReturnPct > 3.0 {
			// Macro window is up, local pullback is a range/consolidation or dip, not macro downtrend
			result.Regime = "RANGE"
		} else {
			result.Regime = "TREND_DOWN"
		}
	}

	// Oscillation overrides:
	// Only override to RANGE if ADX is not in a strong trending state (< 32.0)
	// and Choppiness is not in extreme trending territory (> 38.2).
	isStrongTrend := result.ADX > 32.0 || result.Choppiness < 38.2
	if !isStrongTrend {
		if result.Choppiness >= chopRangeThreshold || midlineCrossings(candles) >= 3 {
			result.Regime = "RANGE"
		}
	}

	// A strong ADX or low Choppiness reading is required to commit a directional grid;
	// weak trend signals keep the neutral grid which is the most robust shape.
	if result.Regime != "RANGE" && (result.ADX < adxTrendThreshold || result.Choppiness > 45.0) {
		result.Regime = "RANGE"
	}
	result.ParkinsonVolatility = parkinsonVolatility(candles, 96.0)
	return result
}

// ChoppinessIndex implements Dreiss Choppiness Index over OHLC candles.
// Values > 61.8 indicate consolidation/ranging; values < 38.2 indicate a strong trend.
func ChoppinessIndex(candles []pionex.KlineCandle, period int) float64 {
	if len(candles) < period+1 || period <= 1 {
		return 50.0
	}
	sumTR := 0.0
	highestHigh := -math.MaxFloat64
	lowestLow := math.MaxFloat64

	start := len(candles) - period
	for i := start; i < len(candles); i++ {
		high, _ := candles[i].High.Float64()
		low, _ := candles[i].Low.Float64()
		prevClose, _ := candles[i-1].Close.Float64()

		tr := math.Max(high-low, math.Max(math.Abs(high-prevClose), math.Abs(low-prevClose)))
		sumTR += tr
		highestHigh = math.Max(highestHigh, high)
		lowestLow = math.Min(lowestLow, low)
	}

	hlRange := highestHigh - lowestLow
	if hlRange <= 0 || sumTR <= 0 {
		return 50.0
	}

	chop := 100.0 * (math.Log10(sumTR/hlRange) / math.Log10(float64(period)))
	return clamp(chop, 0.0, 100.0)
}

// BollingerBandWidth calculates the Bollinger Band Width % and detects volatility squeeze.
func BollingerBandWidth(candles []pionex.KlineCandle, period int, numStd float64) (float64, bool) {
	if len(candles) < period {
		return 0.0, false
	}
	closes := make([]float64, 0, len(candles))
	for _, c := range candles {
		val, _ := c.Close.Float64()
		if val > 0 {
			closes = append(closes, val)
		}
	}
	if len(closes) < period {
		return 0.0, false
	}

	// Calculate SMA and StdDev over the last `period` candles
	window := closes[len(closes)-period:]
	sma := mean(window)
	if sma <= 0 {
		return 0.0, false
	}
	stdDev := sampleStdDev(window)
	bbw := (2 * numStd * stdDev / sma) * 100

	// Check if this is a squeeze (bandwidth is below 2.0% or at local lows)
	isSqueeze := bbw < 2.0
	if len(closes) >= period+10 {
		// Compare with rolling average bandwidth
		prevWindow := closes[len(closes)-period-10 : len(closes)-10]
		prevSma := mean(prevWindow)
		if prevSma > 0 {
			prevStd := sampleStdDev(prevWindow)
			prevBbw := (2 * numStd * prevStd / prevSma) * 100
			if bbw < prevBbw*0.65 {
				isSqueeze = true
			}
		}
	}
	return bbw, isSqueeze
}

// midlineCrossings counts how often the close crosses the midpoint of the
// candle window's high/low range. Trending series cross ~once; oscillating
// series cross on every leg.
func midlineCrossings(candles []pionex.KlineCandle) int {
	if len(candles) < 10 {
		return 0
	}
	start := 0
	if len(candles) > 36 {
		start = len(candles) - 36
	}
	recent := candles[start:]
	low, _ := recent[0].Low.Float64()
	high, _ := recent[0].High.Float64()
	for _, candle := range recent[1:] {
		candleLow, _ := candle.Low.Float64()
		candleHigh, _ := candle.High.Float64()
		low = math.Min(low, candleLow)
		high = math.Max(high, candleHigh)
	}
	midline := (low + high) / 2
	if high <= low {
		return 0
	}
	crossings := 0
	above := false
	initialized := false
	for _, candle := range recent {
		closePrice, _ := candle.Close.Float64()
		currentlyAbove := closePrice > midline
		if initialized && currentlyAbove != above {
			crossings++
		}
		above = currentlyAbove
		initialized = true
	}
	return crossings
}

// RecommendedTrend maps a regime to the Pionex native futures grid trend value.
func (r RegimeResult) RecommendedTrend() string {
	switch r.Regime {
	case "TREND_UP":
		return "long"
	case "TREND_DOWN":
		return "short"
	default:
		return "no_trend"
	}
}

func ema(values []float64, period int) float64 {
	if len(values) == 0 || period <= 0 {
		return 0
	}
	alpha := 2.0 / float64(period+1)
	result := values[0]
	for _, value := range values[1:] {
		result = alpha*value + (1-alpha)*result
	}
	return result
}

// adx implements Wilder's Average Directional Index over OHLC candles.
func adx(candles []pionex.KlineCandle, period int) float64 {
	if len(candles) < period*2+1 {
		return 0
	}
	type smooth struct{ tr, plusDM, minusDM float64 }
	var plusDI, minusDI, trSmooth, plusSmooth, minusSmooth float64
	first := true
	for index := 1; index < len(candles); index++ {
		high, _ := candles[index].High.Float64()
		low, _ := candles[index].Low.Float64()
		prevHigh, _ := candles[index-1].High.Float64()
		prevLow, _ := candles[index-1].Low.Float64()
		prevClose, _ := candles[index-1].Close.Float64()

		tr := math.Max(high-low, math.Max(math.Abs(high-prevClose), math.Abs(low-prevClose)))
		upMove := high - prevHigh
		downMove := prevLow - low
		plusDM, minusDM := 0.0, 0.0
		if upMove > downMove && upMove > 0 {
			plusDM = upMove
		}
		if downMove > upMove && downMove > 0 {
			minusDM = downMove
		}
		if first {
			trSmooth, plusSmooth, minusSmooth = tr, plusDM, minusDM
			first = false
			continue
		}
		// Wilder smoothing.
		trSmooth = trSmooth - trSmooth/float64(period) + tr
		plusSmooth = plusSmooth - plusSmooth/float64(period) + plusDM
		minusSmooth = minusSmooth - minusSmooth/float64(period) + minusDM
	}
	if trSmooth <= 0 {
		return 0
	}
	plusDI = 100 * plusSmooth / trSmooth
	minusDI = 100 * minusSmooth / trSmooth
	sum := plusDI + minusDI
	if sum <= 0 {
		return 0
	}
	dx := 100 * math.Abs(plusDI-minusDI) / sum
	return clamp(dx, 0, 100)
}

func atrPercent(candles []pionex.KlineCandle, period int) float64 {
	if len(candles) < period+1 {
		return 0
	}
	total := 0.0
	counted := 0
	for index := len(candles) - period; index < len(candles); index++ {
		high, _ := candles[index].High.Float64()
		low, _ := candles[index].Low.Float64()
		prevClose, _ := candles[index-1].Close.Float64()
		tr := math.Max(high-low, math.Max(math.Abs(high-prevClose), math.Abs(low-prevClose)))
		total += tr
		counted++
	}
	lastClose, _ := candles[len(candles)-1].Close.Float64()
	if counted == 0 || lastClose <= 0 {
		return 0
	}
	return total / float64(counted) / lastClose * 100
}

func rangePositionPct(candles []pionex.KlineCandle) float64 {
	if len(candles) == 0 {
		return 50
	}
	low, _ := candles[0].Low.Float64()
	high, _ := candles[0].High.Float64()
	for _, candle := range candles[1:] {
		candleLow, _ := candle.Low.Float64()
		candleHigh, _ := candle.High.Float64()
		low = math.Min(low, candleLow)
		high = math.Max(high, candleHigh)
	}
	closePrice, _ := candles[len(candles)-1].Close.Float64()
	if high <= low || closePrice <= 0 {
		return 50
	}
	return clamp((closePrice-low)/(high-low)*100, 0, 100)
}

// supportResistanceRange blends the lookback window's support/resistance with
// the volatility band and an ATR structural buffer so the deployed grid respects
// market structure without clipping right on the edge.
func supportResistanceRange(
	candles []pionex.KlineCandle,
	price, volatilityPct float64,
) (float64, float64) {
	windowLow, windowHigh := math.MaxFloat64, 0.0
	for _, candle := range candles {
		low, _ := candle.Low.Float64()
		high, _ := candle.High.Float64()
		windowLow = math.Min(windowLow, low)
		windowHigh = math.Max(windowHigh, high)
	}
	halfBand := math.Max(volatilityPct, 2.0) / 200
	volLower := price * (1 - halfBand)
	volUpper := price * (1 + halfBand)

	// Add an ATR buffer so the grid bounds have room to absorb local wicks
	atrPct := atrPercent(candles, 14)
	atrBuffer := price * (math.Max(atrPct, 0.5) / 100) * 0.5

	structLower := windowLow - atrBuffer
	structUpper := windowHigh + atrBuffer

	lower := math.Max(structLower, volLower)
	upper := math.Min(structUpper, volUpper)
	if !(lower < price && price < upper && lower > 0) {
		return volLower, volUpper
	}
	return lower, upper
}

// CalculateRSI calculates the Relative Strength Index over closes using Wilder's smoothing.
func CalculateRSI(candles []pionex.KlineCandle, period int) float64 {
	if len(candles) < period+1 || period <= 0 {
		return 50.0
	}
	closes := make([]float64, 0, len(candles))
	for _, c := range candles {
		v, _ := c.Close.Float64()
		if v > 0 {
			closes = append(closes, v)
		}
	}
	if len(closes) < period+1 {
		return 50.0
	}

	var gains, losses float64
	for i := 1; i <= period; i++ {
		change := closes[i] - closes[i-1]
		if change > 0 {
			gains += change
		} else {
			losses -= change
		}
	}
	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	for i := period + 1; i < len(closes); i++ {
		change := closes[i] - closes[i-1]
		var gain, loss float64
		if change > 0 {
			gain = change
		} else {
			loss = -change
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
	}

	if avgLoss == 0 {
		if avgGain == 0 {
			return 50.0
		}
		return 100.0
	}
	rs := avgGain / avgLoss
	return 100.0 - (100.0 / (1.0 + rs))
}
