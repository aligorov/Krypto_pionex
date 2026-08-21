package marketdata

import "math"

// IndicatorBundle carries the independent information classes of the
// confluence engine. Deliberately NOT a stack of correlated oscillators
// (audit debate 2026-08-16): each component measures a different dimension —
// memory/regime (Hurst), volume flow (OBV divergence), one early momentum
// voice (IFT-RSI), fair-price stretch (anchored VWAP) and volatility phase
// (Keltner squeeze).
type IndicatorBundle struct {
	Hurst    float64              // DFA estimate, ~0.5 random walk; <0.45 mean-reverting, >0.58 trending
	HurstOK  bool                 // enough data for a meaningful estimate
	OBVDiv   OBVDivergence        // volume flow versus price pivots
	IFT      IFTRSIResult         // early momentum turn (Vervoort inverse Fisher on RSI)
	AVWAP    AVWAPResult          // stretch versus volume-weighted fair price since anchor
	Keltner  KeltnerSqueeze       // volatility phase: squeeze / release direction
	Fib      FibonacciRetracement // Fibonacci retracement levels & golden pocket detection
	SR       SRAnalysisResult     // Multi-swing support & resistance shelves
	MACD     MACDResult           // MACD (12, 26, 9) line, signal, histogram & crossovers
	StochRSI StochRSIResult       // Stochastic RSI (14, 14, 3, 3) %K/%D and overbought/oversold crosses
	RSIDiv   RSIDivergence        // RSI regular divergence versus price swings
}

// OBVDivergence detects a regular divergence between price pivots and the
// On-Balance Volume line within the lookback: price makes a lower low while
// OBV makes a higher low => accumulation into the decline (bullish), and the
// mirror for distribution (bearish). Divergences against a strong trend are
// the classic trap — the confluence engine never acts on them without the
// Hurst gate and structure context.
type OBVDivergence struct {
	Direction float64 // +1 bullish, -1 bearish, 0 none
	Strength  float64 // 0..1, relative OBV disagreement at the pivots
	PivotAge  int     // bars since the most recent pivot used
}

type IFTRSIResult struct {
	Current     float64 // latest inverse-Fisher value, -1..+1
	Prev        float64
	CrossedUp   bool // crossed above -0.5 from below (early bullish turn)
	CrossedDown bool // crossed below +0.5 from above (early bearish turn)
}

type AVWAPResult struct {
	AnchorIdx int
	Value     float64
	ZScore    float64 // (price - AVWAP) / stdev of deviations, stretched when |z|>1.5
}

type KeltnerSqueeze struct {
	InSqueeze    bool // BB(20,2) fully inside KC(20,1.5×ATR)
	JustReleased bool
	ReleaseDir   int // +1 up, -1 down, 0 none — sign of the release candle
}

type MACDResult struct {
	MACD        float64 `json:"macd"`
	Signal      float64 `json:"signal"`
	Histogram   float64 `json:"histogram"`
	PrevHist    float64 `json:"prevHist"`
	CrossedUp   bool    `json:"crossedUp"`   // Bullish crossover (MACD crossed above Signal)
	CrossedDown bool    `json:"crossedDown"` // Bearish crossover (MACD crossed below Signal)
	HistTurning bool    `json:"histTurning"` // Histogram turning towards zero/opposite direction
}

type StochRSIResult struct {
	K           float64 `json:"k"`
	D           float64 `json:"d"`
	PrevK       float64 `json:"prevK"`
	PrevD       float64 `json:"prevD"`
	Oversold    bool    `json:"oversold"`    // K < 20 && D < 20
	Overbought  bool    `json:"overbought"`  // K > 80 && D > 80
	CrossedUp   bool    `json:"crossedUp"`   // Bullish crossover from oversold zone
	CrossedDown bool    `json:"crossedDown"` // Bearish crossover from overbought zone
}

type RSIDivergence struct {
	Direction float64 `json:"direction"` // +1 bullish, -1 bearish, 0 none
	Strength  float64 `json:"strength"`  // 0..1 relative RSI disagreement
	PivotAge  int     `json:"pivotAge"`  // bars since the most recent pivot
}

// ComputeIndicatorBundle derives all confluence inputs from one series.
func ComputeIndicatorBundle(s *Series) IndicatorBundle {
	bundle := IndicatorBundle{
		IFT: IFTRSIResult{Current: 0, Prev: 0},
	}
	if s == nil || s.Len() < 40 {
		return bundle
	}
	bundle.Hurst, bundle.HurstOK = HurstDFA(s.Close)
	bundle.OBVDiv = DetectOBVDivergence(s.Close, s.Volume, 40)
	bundle.IFT = ComputeIFTRSI(s.Close)
	bundle.AVWAP = ComputeAnchoredVWAP(s)
	bundle.Keltner = DetectKeltnerSqueeze(s)
	bundle.Fib = ComputeFibonacciRetracement(s, 48)
	atr := atrFromSeries(s, s.Len(), 14)
	bundle.SR = DetectSRLevels(s, 60, atr)
	bundle.MACD = ComputeMACD(s.Close)
	bundle.StochRSI = ComputeStochRSI(s.Close)
	bundle.RSIDiv = DetectRSIDivergence(s.Close, 40)
	return bundle
}

// HurstDFA estimates the Hurst exponent with Detrended Fluctuation Analysis
// (linear detrend, DFA1) over log returns. DFA is preferred over R/S for its
// robustness to non-stationarity and outliers; H varies over time, so the
// caller must recompute per scan — never cache across regimes.
func HurstDFA(closes []float64) (float64, bool) {
	n := len(closes)
	if n < 64 {
		return 0.5, false
	}
	returns := make([]float64, n-1)
	for i := 1; i < n; i++ {
		if closes[i-1] <= 0 {
			return 0.5, false
		}
		returns[i-1] = math.Log(closes[i] / closes[i-1])
	}
	// Walk over dyadic-ish scales from 8 to len/4; DFA needs several
	// segments per scale, hence the upper bound.
	maxScale := len(returns) / 4
	if maxScale > 128 {
		maxScale = 128
	}
	scales := make([]int, 0, 6)
	for scale := 8; scale <= maxScale; scale *= 2 {
		scales = append(scales, scale)
	}
	if len(scales) < 3 {
		return 0.5, false
	}
	var logScales, logF []float64
	for _, scale := range scales {
		var sumSq float64
		segments := 0
		for start := 0; start+scale <= len(returns); start += scale {
			segment := returns[start : start+scale]
			mean := 0.0
			for _, v := range segment {
				mean += v
			}
			mean /= float64(scale)
			// Linear detrend via least squares on the cumulative profile.
			profile := make([]float64, scale)
			acc := 0.0
			for i, v := range segment {
				acc += v - mean
				profile[i] = acc
			}
			var sumX, sumY, sumXY, sumXX float64
			for i, v := range profile {
				x := float64(i)
				sumX += x
				sumY += v
				sumXY += x * v
				sumXX += x * x
			}
			denom := float64(scale)*sumXX - sumX*sumX
			if denom == 0 {
				continue
			}
			slope := (float64(scale)*sumXY - sumX*sumY) / denom
			intercept := (sumY - slope*sumX) / float64(scale)
			for i, v := range profile {
				residual := v - (slope*float64(i) + intercept)
				sumSq += residual * residual
			}
			segments++
		}
		if segments == 0 {
			continue
		}
		logScales = append(logScales, math.Log(float64(scale)))
		logF = append(logF, math.Log(math.Sqrt(sumSq/float64(segments*scale))))
	}
	if len(logScales) < 3 {
		return 0.5, false
	}
	// Slope of log F(n) vs log n.
	var sumX, sumY, sumXY, sumXX float64
	for i := range logScales {
		sumX += logScales[i]
		sumY += logF[i]
		sumXY += logScales[i] * logF[i]
		sumXX += logScales[i] * logScales[i]
	}
	k := float64(len(logScales))
	denom := k*sumXX - sumX*sumX
	if denom == 0 {
		return 0.5, false
	}
	hurst := (k*sumXY - sumX*sumY) / denom
	// Guard against pathological estimates from too-short windows.
	if math.IsNaN(hurst) || math.IsInf(hurst, 0) || hurst < 0.1 || hurst > 1.2 {
		return 0.5, false
	}
	return clamp(hurst, 0.1, 1.0), true
}

// DetectOBVDivergence compares the two most recent price swing extremes in
// the lookback window against the OBV values at those bars.
func DetectOBVDivergence(closes, volumes []float64, lookback int) OBVDivergence {
	n := len(closes)
	if n < lookback+2 || len(volumes) != n || lookback < 20 {
		return OBVDivergence{}
	}
	window := n - lookback
	obv := make([]float64, n)
	for i := 1; i < n; i++ {
		obv[i] = obv[i-1]
		switch {
		case closes[i] > closes[i-1]:
			obv[i] += volumes[i]
		case closes[i] < closes[i-1]:
			obv[i] -= volumes[i]
		}
	}
	// Split the lookback into halves and take the minimal close / maximal
	// close of each half as the two pivots (robust, no fractal tuning).
	mid := window + lookback/2
	lowA, lowB := window, mid
	highA, highB := window, mid
	for i := window; i < mid; i++ {
		if closes[i] < closes[lowA] {
			lowA = i
		}
		if closes[i] > closes[highA] {
			highA = i
		}
	}
	for i := mid; i < n; i++ {
		if closes[i] < closes[lowB] {
			lowB = i
		}
		if closes[i] > closes[highB] {
			highB = i
		}
	}
	avgVol := 0.0
	for i := window; i < n; i++ {
		avgVol += volumes[i]
	}
	avgVol /= float64(lookback)
	if avgVol <= 0 {
		return OBVDivergence{}
	}
	result := OBVDivergence{PivotAge: n - 1 - lowB}
	// Bullish: price lower low, OBV higher low.
	if closes[lowB] < closes[lowA] && obv[lowB] > obv[lowA] {
		result.Direction = 1
		result.Strength = clamp((obv[lowB]-obv[lowA])/avgVol/4.0, 0.2, 1)
		result.PivotAge = n - 1 - lowB
		return result
	}
	// Bearish: price higher high, OBV lower high.
	if closes[highB] > closes[highA] && obv[highB] < obv[highA] {
		result.Direction = -1
		result.Strength = clamp((obv[highA]-obv[highB])/avgVol/4.0, 0.2, 1)
		result.PivotAge = n - 1 - highB
		return result
	}
	return result
}

// ComputeIFTRSI implements Vervoort's smoothed-RSI inverse Fisher transform:
// RSI(9) -> EMA(3) -> 0.1*(v-50) -> tanh-style squash. The output spends
// most of its life near the extremes, making ±0.5 crossings sharp and early.
func ComputeIFTRSI(closes []float64) IFTRSIResult {
	result := IFTRSIResult{}
	n := len(closes)
	if n < 12 {
		return result
	}
	rsiSeries := rsiSeries(closes, 9)
	if len(rsiSeries) < 4 {
		return result
	}
	smoothed := emaFloat(rsiSeries, 3)
	value := func(i int) float64 {
		v := 0.1 * (smoothed[i] - 50)
		return math.Tanh(v)
	}
	result.Current = value(len(smoothed) - 1)
	result.Prev = value(len(smoothed) - 2)
	result.CrossedUp = result.Prev <= -0.5 && result.Current > -0.5
	result.CrossedDown = result.Prev >= 0.5 && result.Current < 0.5
	return result
}

// ComputeAnchoredVWAP anchors the volume-weighted average price at the most
// recent major extreme (the window's absolute low or high, whichever came
// later) and reports the current z-score of price versus it.
func ComputeAnchoredVWAP(s *Series) AVWAPResult {
	n := s.Len()
	if n < 30 {
		return AVWAPResult{}
	}
	lowIdx, highIdx := 0, 0
	for i := 1; i < n; i++ {
		if s.Low[i] < s.Low[lowIdx] {
			lowIdx = i
		}
		if s.High[i] > s.High[highIdx] {
			highIdx = i
		}
	}
	anchor := lowIdx
	if highIdx > lowIdx {
		anchor = highIdx
	}
	// A stretch measurement needs a leg with room: when the extreme IS the
	// current bar the anchor degenerates to a one-bar VWAP with zero
	// variance. Fall back to the older extreme, then to a fixed window.
	if n-anchor < 12 {
		anchor = lowIdx
		if n-anchor < 12 {
			anchor = n - 12
		}
		if anchor < 0 {
			anchor = 0
		}
	}
	var cumPV, cumV float64
	for i := anchor; i < n; i++ {
		typical := (s.High[i] + s.Low[i] + s.Close[i]) / 3
		cumPV += typical * s.Volume[i]
		cumV += s.Volume[i]
	}
	if cumV <= 0 {
		return AVWAPResult{}
	}
	avwap := cumPV / cumV
	// Rolling stdev of price deviations from AVWAP since the anchor.
	var sum, sumSq float64
	count := float64(n - anchor)
	for i := anchor; i < n; i++ {
		dev := s.Close[i] - avwap
		sum += dev
		sumSq += dev * dev
	}
	variance := sumSq/count - (sum/count)*(sum/count)
	if variance <= 0 {
		return AVWAPResult{}
	}
	z := (s.Close[n-1] - avwap) / math.Sqrt(variance)
	return AVWAPResult{AnchorIdx: anchor, Value: avwap, ZScore: clamp(z, -5, 5)}
}

// DetectKeltnerSqueeze reports whether the Bollinger band is fully inside
// the Keltner channel (compression) and whether this bar is the release.
func DetectKeltnerSqueeze(s *Series) KeltnerSqueeze {
	n := s.Len()
	if n < 21 {
		return KeltnerSqueeze{}
	}
	bandState := func(end int) (bbLower, bbUpper, kcLower, kcUpper, mid float64) {
		window := s.Close[end-20 : end]
		var sum, sumSq float64
		for _, v := range window {
			sum += v
			sumSq += v * v
		}
		mean := sum / 20
		variance := sumSq/20 - mean*mean
		if variance < 0 {
			variance = 0
		}
		std := math.Sqrt(variance)
		bbLower, bbUpper = mean-2*std, mean+2*std

		// Keltner: EMA(20) of closes ± 1.5 × ATR(20).
		atr := atrFromSeries(s, end, 20)
		kcMid := emaFloatWindow(s.Close[:end], 20)
		kcLower, kcUpper = kcMid-1.5*atr, kcMid+1.5*atr
		mid = mean
		return
	}
	bbLower, bbUpper, kcLower, kcUpper, mid := bandState(n)
	bbLowerPrev, bbUpperPrev, kcLowerPrev, kcUpperPrev, _ := bandState(n - 1)
	inSqueeze := bbUpper < kcUpper && bbLower > kcLower
	inSqueezePrev := bbUpperPrev < kcUpperPrev && bbLowerPrev > kcLowerPrev
	result := KeltnerSqueeze{InSqueeze: inSqueeze}
	if inSqueezePrev && !inSqueeze {
		result.JustReleased = true
		if s.Close[n-1] > mid {
			result.ReleaseDir = 1
		} else if s.Close[n-1] < mid {
			result.ReleaseDir = -1
		}
	}
	return result
}

// rsiSeries computes Wilder RSI over the whole close slice (values aligned
// to the input from index `period`, shifted into a compact slice).
func rsiSeries(closes []float64, period int) []float64 {
	n := len(closes)
	if n < period+1 {
		return nil
	}
	var avgGain, avgLoss float64
	for i := 1; i <= period; i++ {
		change := closes[i] - closes[i-1]
		if change >= 0 {
			avgGain += change
		} else {
			avgLoss -= change
		}
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)
	out := make([]float64, 0, n-period)
	value := func() float64 {
		if avgLoss == 0 {
			if avgGain == 0 {
				return 50
			}
			return 100
		}
		rs := avgGain / avgLoss
		return 100 - 100/(1+rs)
	}
	out = append(out, value())
	for i := period + 1; i < n; i++ {
		change := closes[i] - closes[i-1]
		gain, loss := 0.0, 0.0
		if change >= 0 {
			gain = change
		} else {
			loss = -change
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
		out = append(out, value())
	}
	return out
}

func emaFloat(values []float64, period int) []float64 {
	if len(values) == 0 || period < 1 {
		return values
	}
	k := 2.0 / (float64(period) + 1)
	out := make([]float64, len(values))
	out[0] = values[0]
	for i := 1; i < len(values); i++ {
		out[i] = values[i]*k + out[i-1]*(1-k)
	}
	return out
}

func emaFloatWindow(values []float64, period int) float64 {
	if len(values) == 0 {
		return 0
	}
	smoothed := emaFloat(values[len(values)-period*2:], period)
	if len(smoothed) == 0 {
		return values[len(values)-1]
	}
	return smoothed[len(smoothed)-1]
}

func atrFromSeries(s *Series, end, period int) float64 {
	if end < 2 || period < 1 {
		return 0
	}
	var sum float64
	count := 0
	for i := end - period; i < end; i++ {
		if i < 1 {
			continue
		}
		tr := s.High[i] - s.Low[i]
		if s.High[i]-s.Close[i-1] > tr {
			tr = s.High[i] - s.Close[i-1]
		}
		if s.Close[i-1]-s.Low[i] > tr {
			tr = s.Close[i-1] - s.Low[i]
		}
		sum += tr
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// ComputeMACD computes the classic MACD (12, 26, 9) on closes, identifying line crossover
// and histogram acceleration/reversal patterns.
func ComputeMACD(closes []float64) MACDResult {
	res := MACDResult{}
	n := len(closes)
	if n < 35 {
		return res
	}
	fastEMA := emaFloat(closes, 12)
	slowEMA := emaFloat(closes, 26)
	if len(fastEMA) == 0 || len(slowEMA) == 0 {
		return res
	}
	macdLine := make([]float64, n)
	for i := 0; i < n; i++ {
		macdLine[i] = fastEMA[i] - slowEMA[i]
	}
	signalLine := emaFloat(macdLine, 9)
	if len(signalLine) < 2 {
		return res
	}

	currMACD := macdLine[n-1]
	currSignal := signalLine[n-1]
	prevMACD := macdLine[n-2]
	prevSignal := signalLine[n-2]

	currHist := currMACD - currSignal
	prevHist := prevMACD - prevSignal

	res.MACD = currMACD
	res.Signal = currSignal
	res.Histogram = currHist
	res.PrevHist = prevHist

	// Bullish crossover: MACD crossed above Signal line
	res.CrossedUp = prevMACD <= prevSignal && currMACD > currSignal
	// Bearish crossover: MACD crossed below Signal line
	res.CrossedDown = prevMACD >= prevSignal && currMACD < currSignal

	// Histogram turning: negative histogram shrinking upwards or positive histogram shrinking downwards
	if (prevHist < 0 && currHist > prevHist) || (prevHist > 0 && currHist < prevHist) {
		res.HistTurning = true
	}

	return res
}

// ComputeStochRSI computes the Stochastic RSI (14, 14, 3, 3) over close prices,
// detecting overbought/oversold boundaries and fast %K/%D crossover reversals.
func ComputeStochRSI(closes []float64) StochRSIResult {
	res := StochRSIResult{}
	n := len(closes)
	if n < 32 {
		return res
	}
	rsiVals := rsiSeries(closes, 14)
	nRSI := len(rsiVals)
	if nRSI < 16 {
		return res
	}

	stochSeries := make([]float64, nRSI)
	for i := 13; i < nRSI; i++ {
		minRSI := rsiVals[i]
		maxRSI := rsiVals[i]
		for j := i - 13; j <= i; j++ {
			if rsiVals[j] < minRSI {
				minRSI = rsiVals[j]
			}
			if rsiVals[j] > maxRSI {
				maxRSI = rsiVals[j]
			}
		}
		span := maxRSI - minRSI
		if span <= 0 {
			stochSeries[i] = 50.0
		} else {
			stochSeries[i] = ((rsiVals[i] - minRSI) / span) * 100.0
		}
	}

	// %K = 3-period SMA of StochRSI
	kSeries := make([]float64, nRSI)
	for i := 15; i < nRSI; i++ {
		kSeries[i] = (stochSeries[i] + stochSeries[i-1] + stochSeries[i-2]) / 3.0
	}

	// %D = 3-period SMA of %K
	dSeries := make([]float64, nRSI)
	for i := 17; i < nRSI; i++ {
		dSeries[i] = (kSeries[i] + kSeries[i-1] + kSeries[i-2]) / 3.0
	}

	if nRSI < 19 {
		return res
	}

	currK := kSeries[nRSI-1]
	currD := dSeries[nRSI-1]
	prevK := kSeries[nRSI-2]
	prevD := dSeries[nRSI-2]

	res.K = currK
	res.D = currD
	res.PrevK = prevK
	res.PrevD = prevD

	res.Oversold = currK < 20.0 && currD < 20.0
	res.Overbought = currK > 80.0 && currD > 80.0

	// CrossedUp from low zone (early bullish turn)
	res.CrossedUp = prevK <= prevD && currK > currD && (currK < 35.0 || prevK < 35.0)
	// CrossedDown from high zone (early bearish turn)
	res.CrossedDown = prevK >= prevD && currK < currD && (currK > 65.0 || prevK > 65.0)

	return res
}

// DetectRSIDivergence compares the two most recent price swing extremes in the lookback
// window against the 14-period RSI values at those bars.
func DetectRSIDivergence(closes []float64, lookback int) RSIDivergence {
	n := len(closes)
	if n < lookback+2 || lookback < 20 {
		return RSIDivergence{}
	}
	rsiVals := rsiSeries(closes, 14)
	if len(rsiVals) < lookback {
		return RSIDivergence{}
	}
	window := n - lookback
	if window < 14 {
		window = 14
	}
	mid := window + (n-window)/2
	if mid <= window || mid >= n {
		return RSIDivergence{}
	}

	lowA, lowB := window, mid
	highA, highB := window, mid
	for i := window; i < mid; i++ {
		if closes[i] < closes[lowA] {
			lowA = i
		}
		if closes[i] > closes[highA] {
			highA = i
		}
	}
	for i := mid; i < n; i++ {
		if closes[i] < closes[lowB] {
			lowB = i
		}
		if closes[i] > closes[highB] {
			highB = i
		}
	}

	if lowA < 14 || lowB < 14 || highA < 14 || highB < 14 ||
		lowA-14 >= len(rsiVals) || lowB-14 >= len(rsiVals) ||
		highA-14 >= len(rsiVals) || highB-14 >= len(rsiVals) {
		return RSIDivergence{}
	}

	rsiA_low := rsiVals[lowA-14]
	rsiB_low := rsiVals[lowB-14]
	rsiA_high := rsiVals[highA-14]
	rsiB_high := rsiVals[highB-14]

	result := RSIDivergence{PivotAge: n - 1 - lowB}
	// Bullish: price lower low, RSI higher low
	if closes[lowB] < closes[lowA] && rsiB_low > rsiA_low+1.5 {
		result.Direction = 1
		result.Strength = clamp((rsiB_low-rsiA_low)/25.0, 0.2, 1.0)
		result.PivotAge = n - 1 - lowB
		return result
	}
	// Bearish: price higher high, RSI lower high
	if closes[highB] > closes[highA] && rsiB_high < rsiA_high-1.5 {
		result.Direction = -1
		result.Strength = clamp((rsiA_high-rsiB_high)/25.0, 0.2, 1.0)
		result.PivotAge = n - 1 - highB
		return result
	}
	return result
}

