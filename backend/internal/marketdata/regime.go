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
	BBWPercentile       float64 `json:"bbwPercentile"`       // BBW rank within the window, 0..100 (low = tighter than most of the history)
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
	result.BBWPercentile = bbwPercentileRank(candles, bbPeriod)
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
		if windowReturnPct < -3.0 {
			// Mirror of the short-side macro guard: the macro window is down,
			// so a local up-pullback is a relief rally in a downtrend, not an
			// uptrend. Without this the engine longs into obvious shorts.
			result.Regime = "RANGE"
		} else {
			result.Regime = "TREND_UP"
		}
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

// adx implements Wilder's Average Directional Index over OHLC candles:
// +DI/-DI are Wilder-smoothed, DX is derived per candle, and ADX is the
// Wilder-smoothed average of the DX series. Returning raw DX (as this
// function did before) made the trend thresholds classify noise as trend
// and trend as noise.
func adx(candles []pionex.KlineCandle, period int) float64 {
	if len(candles) < period*2+1 {
		return 0
	}
	var trSmooth, plusSmooth, minusSmooth float64
	dxs := make([]float64, 0, len(candles))
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

		if trSmooth > 0 {
			plusDI := 100 * plusSmooth / trSmooth
			minusDI := 100 * minusSmooth / trSmooth
			sum := plusDI + minusDI
			if sum > 0 {
				dxs = append(dxs, clamp(100*math.Abs(plusDI-minusDI)/sum, 0, 100))
			}
		}
	}
	if len(dxs) == 0 {
		return 0
	}
	if len(dxs) < period {
		return mean(dxs)
	}
	// Wilder ADX: seed with the mean of the first `period` DX values, then
	// smooth each subsequent DX with the (period-1)/period recursion.
	result := mean(dxs[:period])
	for _, dx := range dxs[period:] {
		result = (result*float64(period-1) + dx) / float64(period)
	}
	return clamp(result, 0, 100)
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
// bbwPercentileRank ranks the current Bollinger Band Width against the
// widths computed over the whole window: 20 means bands are currently
// tighter than 80% of the window — the documented entry regime for neutral
// grids ("band width below its 20th percentile"). A rolling-average squeeze
// flag only compares against the recent mean; the rank places the reading
// in the full distribution.
func bbwPercentileRank(candles []pionex.KlineCandle, period int) float64 {
	if len(candles) < period+12 {
		return 50.0
	}
	// Second return of BollingerBandWidth is the squeeze flag, not an
	// ok-marker; the value is always computed once the window fits.
	current, _ := BollingerBandWidth(candles, period, 2.0)
	samples := 0
	atOrBelow := 0
	for end := period + 2; end <= len(candles); end++ {
		historical, _ := BollingerBandWidth(candles[:end], period, 2.0)
		samples++
		if historical <= current {
			atOrBelow++
		}
	}
	if samples < 10 {
		return 50.0
	}
	return float64(atOrBelow) / float64(samples) * 100.0
}

// volumeProfileBounds returns the price band that contains `cover` (0..1)
// of the window's traded volume, expanded outward from the highest-volume
// bin. Grid bounds anchored on real volume nodes are defended by actual
// traded liquidity instead of raw candle extremes, which a single wick can
// inflate.
func volumeProfileBounds(candles []pionex.KlineCandle, cover float64) (float64, float64, bool) {
	if len(candles) < 10 || cover <= 0 || cover > 1 {
		return 0, 0, false
	}
	const bins = 48
	windowLow, windowHigh := math.MaxFloat64, 0.0
	totalVolume := 0.0
	for _, candle := range candles {
		low, _ := candle.Low.Float64()
		high, _ := candle.High.Float64()
		volume, _ := candle.Volume.Float64()
		if high <= 0 || low <= 0 || volume <= 0 {
			continue
		}
		windowLow = math.Min(windowLow, low)
		windowHigh = math.Max(windowHigh, high)
		totalVolume += volume
	}
	if totalVolume <= 0 || windowHigh <= windowLow {
		return 0, 0, false
	}
	histogram := make([]float64, bins)
	binWidth := (windowHigh - windowLow) / bins
	if binWidth <= 0 {
		return 0, 0, false
	}
	for _, candle := range candles {
		low, _ := candle.Low.Float64()
		high, _ := candle.High.Float64()
		volume, _ := candle.Volume.Float64()
		if volume <= 0 || high <= 0 || low <= 0 {
			continue
		}
		// Spread each candle's volume uniformly over the bins it spans.
		// Both ends are clamped: a doji candle sitting exactly on the window
		// high (low == high == windowHigh) makes the ratio land exactly on
		// `bins` after floor, which would index one past the histogram.
		fromBin := int(math.Floor((low - windowLow) / binWidth))
		if fromBin < 0 {
			fromBin = 0
		}
		if fromBin > bins-1 {
			fromBin = bins - 1
		}
		toBin := int(math.Floor((high - windowLow) / binWidth))
		if toBin > bins-1 {
			toBin = bins - 1
		}
		if toBin < fromBin {
			toBin = fromBin
		}
		perBin := volume / float64(toBin-fromBin+1)
		for b := fromBin; b <= toBin; b++ {
			histogram[b] += perBin
		}
	}
	peak := 0
	for b := 1; b < bins; b++ {
		if histogram[b] > histogram[peak] {
			peak = b
		}
	}
	target := totalVolume * cover
	accumulated := histogram[peak]
	lowerBin, upperBin := peak, peak
	for accumulated < target && (lowerBin > 0 || upperBin < bins-1) {
		nextLower := 0.0
		if lowerBin > 0 {
			nextLower = histogram[lowerBin-1]
		}
		nextUpper := 0.0
		if upperBin < bins-1 {
			nextUpper = histogram[upperBin+1]
		}
		if nextLower >= nextUpper && nextLower > 0 {
			lowerBin--
			accumulated += nextLower
		} else if nextUpper > 0 {
			upperBin++
			accumulated += nextUpper
		} else if nextLower > 0 {
			lowerBin--
			accumulated += nextLower
		} else {
			break
		}
	}
	vpLower := windowLow + float64(lowerBin)*binWidth
	vpUpper := windowLow + float64(upperBin+1)*binWidth
	if vpUpper <= vpLower {
		return 0, 0, false
	}
	return vpLower, vpUpper, true
}

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

	// Anchor the bounds on real traded volume where the profile agrees:
	// wick-inflated extremes get pulled back to the band where liquidity
	// actually changed hands. The result must still bracket the price and
	// keep a sane minimum span — otherwise the structural bounds stand.
	if vpLower, vpUpper, ok := volumeProfileBounds(candles, 0.7); ok {
		tighterLower := math.Max(lower, vpLower-atrBuffer)
		tighterUpper := math.Min(upper, vpUpper+atrBuffer)
		if tighterLower < price && price < tighterUpper &&
			tighterUpper-tighterLower >= price*0.02 {
			lower, upper = tighterLower, tighterUpper
		}
	}

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
