package marketdata

import (
	"math"
	"sort"
)

// SRLevel represents an identified support or resistance price shelf with touch count and strength.
type SRLevel struct {
	Price    float64 `json:"price"`
	Touches  int     `json:"touches"`
	Strength float64 `json:"strength"` // 0..1 normalized strength score
	AgeBars  int     `json:"ageBars"`  // bars since the most recent touch
}

// SRAnalysisResult contains clustered support and resistance levels for a price series.
type SRAnalysisResult struct {
	Supports        []SRLevel `json:"supports"`        // Sorted descending by price (nearest below current price first)
	Resistances     []SRLevel `json:"resistances"`     // Sorted ascending by price (nearest above current price first)
	NearestSupport  float64   `json:"nearestSupport"`  // 0 if none found
	NearestResist   float64   `json:"nearestResist"`   // 0 if none found
	SupportDistPct  float64   `json:"supportDistPct"`  // (Price - NearestSupport) / Price * 100
	ResistDistPct   float64   `json:"resistDistPct"`   // (NearestResist - Price) / Price * 100
	SupportStrength float64   `json:"supportStrength"` // 0..1
	ResistStrength  float64   `json:"resistStrength"`  // 0..1
}

// DetectSRLevels detects and clusters structural swing highs and lows into support and resistance shelves.
func DetectSRLevels(s *Series, lookback int, atr float64) SRAnalysisResult {
	res := SRAnalysisResult{}
	if s == nil || s.Len() < 15 {
		return res
	}
	n := s.Len()
	if lookback <= 0 || lookback > n {
		lookback = n
	}
	start := n - lookback
	if start < 1 {
		start = 1
	}

	currPrice := s.Close[n-1]
	if currPrice <= 0 {
		return res
	}

	// Clustering tolerance: 0.65 * ATR or 0.6% of current price
	tol := 0.006 * currPrice
	if atr > 0 {
		tol = math.Max(0.65*atr, 0.003*currPrice)
	}

	type pivotPoint struct {
		price float64
		idx   int
	}
	var swingHighs []pivotPoint
	var swingLows []pivotPoint

	// Radius of 2 bars for local extremum detection
	for i := start + 2; i < n-2; i++ {
		// Local High
		if s.High[i] >= s.High[i-1] && s.High[i] >= s.High[i-2] &&
			s.High[i] >= s.High[i+1] && s.High[i] >= s.High[i+2] {
			swingHighs = append(swingHighs, pivotPoint{price: s.High[i], idx: i})
		}
		// Local Low
		if s.Low[i] <= s.Low[i-1] && s.Low[i] <= s.Low[i-2] &&
			s.Low[i] <= s.Low[i+1] && s.Low[i] <= s.Low[i+2] {
			swingLows = append(swingLows, pivotPoint{price: s.Low[i], idx: i})
		}
	}

	// Helper to cluster pivots
	clusterPivots := func(pivots []pivotPoint) []SRLevel {
		if len(pivots) == 0 {
			return nil
		}
		type cluster struct {
			prices  []float64
			indices []int
		}
		var clusters []cluster

		for _, p := range pivots {
			matched := false
			for j := range clusters {
				// Check distance to cluster center
				center := 0.0
				for _, pr := range clusters[j].prices {
					center += pr
				}
				center /= float64(len(clusters[j].prices))
				if math.Abs(p.price-center) <= tol {
					clusters[j].prices = append(clusters[j].prices, p.price)
					clusters[j].indices = append(clusters[j].indices, p.idx)
					matched = true
					break
				}
			}
			if !matched {
				clusters = append(clusters, cluster{
					prices:  []float64{p.price},
					indices: []int{p.idx},
				})
			}
		}

		var levels []SRLevel
		for _, cl := range clusters {
			sum := 0.0
			maxIdx := 0
			for j, pr := range cl.prices {
				sum += pr
				if cl.indices[j] > maxIdx {
					maxIdx = cl.indices[j]
				}
			}
			avgPrice := sum / float64(len(cl.prices))
			touches := len(cl.prices)
			// Strength scales with touches (1 touch = 0.35, 2 = 0.65, 3+ = 0.90+)
			strength := clamp(0.35+float64(touches-1)*0.25, 0.2, 1.0)
			age := n - 1 - maxIdx
			levels = append(levels, SRLevel{
				Price:    avgPrice,
				Touches:  touches,
				Strength: strength,
				AgeBars:  age,
			})
		}
		return levels
	}

	highLevels := clusterPivots(swingHighs)
	lowLevels := clusterPivots(swingLows)
	allLevels := append(highLevels, lowLevels...)

	var supports []SRLevel
	var resistances []SRLevel

	for _, lvl := range allLevels {
		if lvl.Price < currPrice*(1.0-0.001) {
			supports = append(supports, lvl)
		} else if lvl.Price > currPrice*(1.0+0.001) {
			resistances = append(resistances, lvl)
		}
	}

	// Sort supports descending (closest to current price first)
	sort.Slice(supports, func(i, j int) bool {
		return supports[i].Price > supports[j].Price
	})
	// Sort resistances ascending (closest to current price first)
	sort.Slice(resistances, func(i, j int) bool {
		return resistances[i].Price < resistances[j].Price
	})

	// Deduplicate close levels in supports
	res.Supports = deduplicateSR(supports, tol)
	res.Resistances = deduplicateSR(resistances, tol)

	if len(res.Supports) > 0 {
		res.NearestSupport = res.Supports[0].Price
		res.SupportDistPct = ((currPrice - res.NearestSupport) / currPrice) * 100.0
		res.SupportStrength = res.Supports[0].Strength
	}
	if len(res.Resistances) > 0 {
		res.NearestResist = res.Resistances[0].Price
		res.ResistDistPct = ((res.NearestResist - currPrice) / currPrice) * 100.0
		res.ResistStrength = res.Resistances[0].Strength
	}

	return res
}

func deduplicateSR(levels []SRLevel, tol float64) []SRLevel {
	if len(levels) <= 1 {
		return levels
	}
	var out []SRLevel
	for _, lvl := range levels {
		duplicate := false
		for k := range out {
			if math.Abs(lvl.Price-out[k].Price) <= tol {
				// Merge touches and take stronger
				out[k].Touches += lvl.Touches
				if lvl.Strength > out[k].Strength {
					out[k].Strength = lvl.Strength
				}
				if lvl.AgeBars < out[k].AgeBars {
					out[k].AgeBars = lvl.AgeBars
				}
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, lvl)
		}
	}
	return out
}
