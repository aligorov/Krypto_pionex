package marketdata

import "math"

// FibonacciRetracement holds calculated Fibonacci levels and zone information for a price series.
type FibonacciRetracement struct {
	SwingHigh       float64    `json:"swingHigh"`
	SwingLow        float64    `json:"swingLow"`
	TrendDir        int        `json:"trendDir"`        // +1 upswing (pullback down), -1 downswing (relief rally up), 0 undefined
	Levels          [5]float64 `json:"levels"`          // Price levels at ratios [0.236, 0.382, 0.500, 0.618, 0.786]
	Ratios          [5]float64 `json:"ratios"`          // [0.236, 0.382, 0.500, 0.618, 0.786]
	CurrentPrice    float64    `json:"currentPrice"`
	InGoldenPocket  bool       `json:"inGoldenPocket"`  // true if price is in 0.618-0.786 retracement zone
	NearLevel       float64    `json:"nearLevel"`       // closest Fibonacci price level
	NearRatio       float64    `json:"nearRatio"`       // ratio corresponding to NearLevel
	DistancePct     float64    `json:"distancePct"`     // |Price - NearLevel| / Price * 100
	SupportLevel    float64    `json:"supportLevel"`    // closest Fib level below current price
	ResistanceLevel float64    `json:"resistanceLevel"` // closest Fib level above current price
}

var fibRatios = [5]float64{0.236, 0.382, 0.500, 0.618, 0.786}

// ComputeFibonacciRetracement computes Fibonacci retracement levels from the most recent swing leg
// in the lookback window of the Series.
func ComputeFibonacciRetracement(s *Series, lookback int) FibonacciRetracement {
	res := FibonacciRetracement{
		Ratios: fibRatios,
	}
	if s == nil || s.Len() < 10 {
		return res
	}
	n := s.Len()
	if lookback <= 0 || lookback > n {
		lookback = n
	}
	start := n - lookback
	if start < 0 {
		start = 0
	}

	// Find absolute swing high and swing low in the lookback window
	highIdx := start
	lowIdx := start
	for i := start; i < n; i++ {
		if s.High[i] > s.High[highIdx] {
			highIdx = i
		}
		if s.Low[i] < s.Low[lowIdx] {
			lowIdx = i
		}
	}

	swingHigh := s.High[highIdx]
	swingLow := s.Low[lowIdx]
	currPrice := s.Close[n-1]

	res.SwingHigh = swingHigh
	res.SwingLow = swingLow
	res.CurrentPrice = currPrice

	span := swingHigh - swingLow
	if span <= 0 || currPrice <= 0 {
		return res
	}

	// Determine dominant swing direction based on which extreme occurred first
	if lowIdx < highIdx {
		// Low came before High -> Upswing leg (bullish impulse).
		// Retracement moves DOWN from SwingHigh towards SwingLow.
		res.TrendDir = 1
		for i, r := range fibRatios {
			// Pullback from High: Price = High - span * ratio
			res.Levels[i] = swingHigh - span*r
		}
		// Golden pocket in upswing pullback is between 0.618 and 0.786 retrace:
		// level[3] is 0.618 (higher price), level[4] is 0.786 (lower price)
		res.InGoldenPocket = currPrice <= res.Levels[3]*1.002 && currPrice >= res.Levels[4]*0.998
	} else if highIdx < lowIdx {
		// High came before Low -> Downswing leg (bearish impulse).
		// Retracement moves UP from SwingLow towards SwingHigh.
		res.TrendDir = -1
		for i, r := range fibRatios {
			// Relief bounce from Low: Price = Low + span * ratio
			res.Levels[i] = swingLow + span*r
		}
		// Golden pocket in downswing relief is between 0.618 and 0.786 retrace:
		// level[3] is 0.618 (lower price), level[4] is 0.786 (higher price)
		res.InGoldenPocket = currPrice >= res.Levels[3]*0.998 && currPrice <= res.Levels[4]*1.002
	} else {
		// Degenerate single-bar extreme
		res.TrendDir = 0
		for i, r := range fibRatios {
			res.Levels[i] = swingLow + span*r
		}
	}

	// Identify nearest Fib level and distance
	minDist := math.MaxFloat64
	bestLevel := res.Levels[0]
	bestRatio := fibRatios[0]
	for i, lvl := range res.Levels {
		dist := math.Abs(currPrice - lvl)
		if dist < minDist {
			minDist = dist
			bestLevel = lvl
			bestRatio = fibRatios[i]
		}
	}
	res.NearLevel = bestLevel
	res.NearRatio = bestRatio
	res.DistancePct = (minDist / currPrice) * 100.0

	// Identify closest support (highest Fib level < currPrice) and resistance (lowest Fib level > currPrice)
	closestSup := 0.0
	closestRes := math.MaxFloat64
	for _, lvl := range res.Levels {
		if lvl < currPrice && lvl > closestSup {
			closestSup = lvl
		}
		if lvl > currPrice && lvl < closestRes {
			closestRes = lvl
		}
	}
	if closestSup > 0 {
		res.SupportLevel = closestSup
	} else {
		res.SupportLevel = swingLow
	}
	if closestRes < math.MaxFloat64 {
		res.ResistanceLevel = closestRes
	} else {
		res.ResistanceLevel = swingHigh
	}

	return res
}
