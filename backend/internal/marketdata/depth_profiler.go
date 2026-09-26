package marketdata

import (
	"math"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// DepthProfile encapsulates the liquidity structure and wall density of the order book.
type DepthProfile struct {
	BidVolumeUSDT    float64         `json:"bidVolumeUsdt"`
	AskVolumeUSDT    float64         `json:"askVolumeUsdt"`
	ImbalanceRatio   float64         `json:"imbalanceRatio"`   // Bids / (Bids + Asks), > 0.5 means bid heavy
	BidCushionRatio  float64         `json:"bidCushionRatio"`  // Bid volume in 2% / bot notional
	AskCushionRatio  float64         `json:"askCushionRatio"`  // Ask volume in 2% / bot notional
	BidWallPrice     decimal.Decimal `json:"bidWallPrice"`     // Nearest significant bid wall
	BidWallNotional  float64         `json:"bidWallNotional"`
	AskWallPrice     decimal.Decimal `json:"askWallPrice"`     // Nearest significant ask wall
	AskWallNotional  float64         `json:"askWallNotional"`
	IsThinBook       bool            `json:"isThinBook"`       // True if cushion is below threshold
	HasBidWall       bool            `json:"hasBidWall"`
	HasAskWall       bool            `json:"hasAskWall"`
}

// ProfileOrderBook analyzes the L2 aggregated depth levels around current price.
func ProfileOrderBook(
	bids []pionex.DepthLevel,
	asks []pionex.DepthLevel,
	currentPrice decimal.Decimal,
	botNotional float64,
	minCushionRatio float64,
) DepthProfile {
	priceF, _ := currentPrice.Float64()
	if priceF <= 0 {
		return DepthProfile{}
	}
	if minCushionRatio <= 0 {
		minCushionRatio = 50.0 // Default 50x cushion
	}

	// 2% depth threshold
	bidThreshold := priceF * 0.98
	askThreshold := priceF * 1.02

	var bidVol2Pct float64
	for _, b := range bids {
		pF, _ := b.Price.Float64()
		aF, _ := b.Amount.Float64()
		if pF >= bidThreshold && pF <= priceF {
			bidVol2Pct += pF * aF
		}
	}

	var askVol2Pct float64
	for _, a := range asks {
		pF, _ := a.Price.Float64()
		aF, _ := a.Amount.Float64()
		if pF <= askThreshold && pF >= priceF {
			askVol2Pct += pF * aF
		}
	}

	totalVol := bidVol2Pct + askVol2Pct
	imbalance := 0.5
	if totalVol > 0 {
		imbalance = bidVol2Pct / totalVol
	}

	bidCushion := 999.0
	askCushion := 999.0
	if botNotional > 0 {
		bidCushion = bidVol2Pct / botNotional
		askCushion = askVol2Pct / botNotional
	}

	// Find order walls within 5%
	bidWallPrice, bidWallNotional, hasBidWall := FindMajorWall(bids, currentPrice, true, 0.05)
	askWallPrice, askWallNotional, hasAskWall := FindMajorWall(asks, currentPrice, false, 0.05)

	isThin := false
	if botNotional > 0 && bidCushion < minCushionRatio {
		isThin = true
	}

	return DepthProfile{
		BidVolumeUSDT:   bidVol2Pct,
		AskVolumeUSDT:   askVol2Pct,
		ImbalanceRatio:  imbalance,
		BidCushionRatio: bidCushion,
		AskCushionRatio: askCushion,
		BidWallPrice:    bidWallPrice,
		BidWallNotional: bidWallNotional,
		AskWallPrice:    askWallPrice,
		AskWallNotional: askWallNotional,
		IsThinBook:      isThin,
		HasBidWall:      hasBidWall,
		HasAskWall:      hasAskWall,
	}
}

// FindMajorWall locates the most significant limit order wall within maxDistancePct of price.
// A level qualifies as a "wall" if its notional is >= 3x the median level notional in the book.
func FindMajorWall(
	levels []pionex.DepthLevel,
	currentPrice decimal.Decimal,
	isBid bool,
	maxDistancePct float64,
) (wallPrice decimal.Decimal, wallNotional float64, found bool) {
	priceF, _ := currentPrice.Float64()
	if priceF <= 0 || len(levels) == 0 {
		return decimal.Zero, 0, false
	}

	limitLow := priceF * (1.0 - maxDistancePct)
	limitHigh := priceF * (1.0 + maxDistancePct)

	var notionals []float64
	for _, l := range levels {
		pF, _ := l.Price.Float64()
		aF, _ := l.Amount.Float64()
		if isBid {
			if pF >= limitLow && pF <= priceF {
				notionals = append(notionals, pF*aF)
			}
		} else {
			if pF <= limitHigh && pF >= priceF {
				notionals = append(notionals, pF*aF)
			}
		}
	}

	if len(notionals) < 3 {
		return decimal.Zero, 0, false
	}

	// Compute average notional of surrounding levels
	var sum float64
	for _, n := range notionals {
		sum += n
	}
	avgNotional := sum / float64(len(notionals))

	var maxN float64
	var bestPrice decimal.Decimal
	for _, l := range levels {
		pF, _ := l.Price.Float64()
		aF, _ := l.Amount.Float64()
		n := pF * aF
		inRange := false
		if isBid && pF >= limitLow && pF <= priceF {
			inRange = true
		} else if !isBid && pF <= limitHigh && pF >= priceF {
			inRange = true
		}
		if inRange && n > maxN && n >= 2.5*avgNotional && n >= 5000.0 { // at least $5,000 and 2.5x average
			maxN = n
			bestPrice = l.Price
		}
	}

	if maxN > 0 {
		return bestPrice, math.Round(maxN), true
	}
	return decimal.Zero, 0, false
}
