package marketdata

import (
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// TakerFlowMetrics summarizes the recent aggressive market taker order flow.
type TakerFlowMetrics struct {
	BuyVolumeUSDT       float64 `json:"buyVolumeUsdt"`
	SellVolumeUSDT      float64 `json:"sellVolumeUsdt"`
	TotalVolumeUSDT     float64 `json:"totalVolumeUsdt"`
	BuyRatio            float64 `json:"buyRatio"`       // BuyVol / TotalVol
	SellRatio           float64 `json:"sellRatio"`      // SellVol / TotalVol
	TradeCount          int     `json:"tradeCount"`
	LargeSellCount      int     `json:"largeSellCount"` // Sells > $5,000
	LargeBuyCount       int     `json:"largeBuyCount"`  // Buys > $5,000
	IsAggressiveDumping bool    `json:"isAggressiveDumping"`
	IsAggressivePumping bool    `json:"isAggressivePumping"`
}

// AnalyzeTakerFlow calculates volume-weighted taker side dominance from recent trades.
func AnalyzeTakerFlow(trades []pionex.Trade) TakerFlowMetrics {
	metrics := TakerFlowMetrics{TradeCount: len(trades)}
	if len(trades) == 0 {
		return metrics
	}

	for _, t := range trades {
		pF, _ := t.Price.Float64()
		sF, _ := t.Size.Float64()
		notional := pF * sF
		side := strings.ToUpper(t.Side)

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

	// Aggressive dumping: >= 75% of volume is market sell AND total volume >= $15,000
	if metrics.SellRatio >= 0.75 && metrics.SellVolumeUSDT >= 15000.0 {
		metrics.IsAggressiveDumping = true
	}

	// Aggressive pumping: >= 75% of volume is market buy AND total volume >= $15,000
	if metrics.BuyRatio >= 0.75 && metrics.BuyVolumeUSDT >= 15000.0 {
		metrics.IsAggressivePumping = true
	}

	return metrics
}
