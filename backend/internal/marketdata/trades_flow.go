package marketdata

import (
	"math"
	"sort"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// TakerFlowMetrics summarizes the recent aggressive market taker order flow.
type TakerFlowMetrics struct {
	BuyVolumeUSDT       float64 `json:"buyVolumeUsdt"`
	SellVolumeUSDT      float64 `json:"sellVolumeUsdt"`
	TotalVolumeUSDT     float64 `json:"totalVolumeUsdt"`
	BuyRatio            float64 `json:"buyRatio"`  // BuyVol / TotalVol
	SellRatio           float64 `json:"sellRatio"` // SellVol / TotalVol
	TradeCount          int     `json:"tradeCount"`
	LargeSellCount      int     `json:"largeSellCount"` // Sells > $5,000 (legacy threshold)
	LargeBuyCount       int     `json:"largeBuyCount"`  // Buys > $5,000 (legacy threshold)
	MedianNotionalUSDT  float64 `json:"medianNotionalUsdt"`
	MADNotionalUSDT     float64 `json:"madNotionalUsdt"`
	WhaleBuyCount       int     `json:"whaleBuyCount"`  // Buys with Modified Z-Score >= 3.5 & notional >= $500
	WhaleSellCount      int     `json:"whaleSellCount"` // Sells with Modified Z-Score >= 3.5 & notional >= $500
	WhaleBuyVolumeUSDT  float64 `json:"whaleBuyVolumeUsdt"`
	WhaleSellVolumeUSDT float64 `json:"whaleSellVolumeUsdt"`
	MaxModifiedZScore   float64 `json:"maxModifiedZScore"`
	IsAggressiveDumping bool    `json:"isAggressiveDumping"`
	IsAggressivePumping bool    `json:"isAggressivePumping"`
}

// medianFloat64 computes the median of a float64 slice without modifying input order.
func medianFloat64(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2.0
}

// computeMAD calculates Median Absolute Deviation: median(|x_i - median(x)|).
// A relative dispersion floor prevents division by zero or extreme sensitivity
// when identical trade sizes cluster.
func computeMAD(vals []float64, med float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	devs := make([]float64, n)
	for i, v := range vals {
		devs[i] = math.Abs(v - med)
	}
	mad := medianFloat64(devs)
	floor := 0.05 * med
	if floor <= 0 {
		floor = 1.0
	}
	if mad < floor {
		mad = floor
	}
	return mad
}

// AnalyzeTakerFlow calculates volume-weighted taker side dominance from recent trades
// using both classical thresholds and scale-invariant Median Absolute Deviation (MAD)
// Modified Z-Scores from quantitative whale detection.
func AnalyzeTakerFlow(trades []pionex.Trade) TakerFlowMetrics {
	metrics := TakerFlowMetrics{TradeCount: len(trades)}
	if len(trades) == 0 {
		return metrics
	}

	tradeNotionals := make([]float64, len(trades))
	tradeSides := make([]string, len(trades))

	for i, t := range trades {
		pF, _ := t.Price.Float64()
		sF, _ := t.Size.Float64()
		notional := pF * sF
		side := strings.ToUpper(t.Side)

		tradeNotionals[i] = notional
		tradeSides[i] = side

		if side == "BUY" {
			metrics.BuyVolumeUSDT += notional
			if notional >= 5000.0 {
				metrics.LargeBuyCount++
			}
		} else if side == "SELL" {
			metrics.SellVolumeUSDT += notional
			if notional >= 5000.0 {
				metrics.LargeSellCount++
			}
		}
	}

	metrics.TotalVolumeUSDT = metrics.BuyVolumeUSDT + metrics.SellVolumeUSDT
	if metrics.TotalVolumeUSDT > 0 {
		metrics.BuyRatio = metrics.BuyVolumeUSDT / metrics.TotalVolumeUSDT
		metrics.SellRatio = metrics.SellVolumeUSDT / metrics.TotalVolumeUSDT
	}

	// 1. Classical static threshold: >= 75% of volume is market sell AND total volume >= $15,000
	if metrics.SellRatio >= 0.75 && metrics.SellVolumeUSDT >= 15000.0 {
		metrics.IsAggressiveDumping = true
	}

	// 2. Classical static threshold: >= 75% of volume is market buy AND total volume >= $15,000
	if metrics.BuyRatio >= 0.75 && metrics.BuyVolumeUSDT >= 15000.0 {
		metrics.IsAggressivePumping = true
	}

	// 3. Robust MAD Modified Z-score Whale Detection
	// M_z = 0.6745 * (x - median) / MAD
	metrics.MedianNotionalUSDT = medianFloat64(tradeNotionals)
	metrics.MADNotionalUSDT = computeMAD(tradeNotionals, metrics.MedianNotionalUSDT)

	for i, notional := range tradeNotionals {
		mz := 0.6745 * (notional - metrics.MedianNotionalUSDT) / metrics.MADNotionalUSDT
		if mz > metrics.MaxModifiedZScore {
			metrics.MaxModifiedZScore = mz
		}

		// A trade qualifies as a whale outlier if M_z >= 3.5 AND notional >= $500
		// (noise floor prevents tiny $30 trades from being flagged on dead books).
		if mz >= 3.5 && notional >= 500.0 {
			if tradeSides[i] == "BUY" {
				metrics.WhaleBuyCount++
				metrics.WhaleBuyVolumeUSDT += notional
			} else if tradeSides[i] == "SELL" {
				metrics.WhaleSellCount++
				metrics.WhaleSellVolumeUSDT += notional
			}
		}
	}

	// 4. Robust Whale Outlier Dumping / Pumping:
	// If the sell ratio dominates (>= 70%), total sell >= $2,000, and whale sell volume constitutes >= 40%
	// of sell volume or at least 2 whale dump orders were detected.
	if !metrics.IsAggressiveDumping && metrics.SellRatio >= 0.70 && metrics.SellVolumeUSDT >= 2000.0 {
		if (metrics.WhaleSellVolumeUSDT >= 0.40*metrics.SellVolumeUSDT && metrics.WhaleSellCount >= 1) || metrics.WhaleSellCount >= 2 {
			metrics.IsAggressiveDumping = true
		}
	}

	if !metrics.IsAggressivePumping && metrics.BuyRatio >= 0.70 && metrics.BuyVolumeUSDT >= 2000.0 {
		if (metrics.WhaleBuyVolumeUSDT >= 0.40*metrics.BuyVolumeUSDT && metrics.WhaleBuyCount >= 1) || metrics.WhaleBuyCount >= 2 {
			metrics.IsAggressivePumping = true
		}
	}

	return metrics
}
