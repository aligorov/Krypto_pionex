package autogrid

import (
	"math"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

type ConfluenceEvaluation struct {
	Score              float64  `json:"score"`              // 0 to 100
	IsFired            bool     `json:"isFired"`            // True if Score >= 75
	Status             string   `json:"status"`             // 'ARMED', 'WAITING', 'OVERHEATED', 'TRIGGERED'
	Factor1LevelScore  float64  `json:"factor1LevelScore"`  // max 35
	Factor2MomentumScore float64 `json:"factor2MomentumScore"`// max 35
	Factor3CandleScore float64  `json:"factor3CandleScore"` // max 30
	RejectionReasons   []string `json:"rejectionReasons"`
	TargetEntryPrice   decimal.Decimal `json:"targetEntryPrice"`
	DistanceToEntryPct float64  `json:"distanceToEntryPct"`
}

// EvaluateConfluence evaluates a candidate pair against the 3-factor institutional confluence model.
func EvaluateConfluence(
	candidate Candidate,
	candles []pionex.KlineCandle,
	fundingRate *decimal.Decimal,
) ConfluenceEvaluation {
	eval := ConfluenceEvaluation{
		RejectionReasons: make([]string, 0),
	}

	rangePos := 50.0
	if val, ok := candidate.ModelAssumptions["rangePositionPct"].(float64); ok {
		rangePos = val
	} else if candidate.UpperPrice.GreaterThan(candidate.LowerPrice) && candidate.CurrentPrice.GreaterThan(decimal.Zero) {
		span := candidate.UpperPrice.Sub(candidate.LowerPrice)
		offset := candidate.CurrentPrice.Sub(candidate.LowerPrice)
		p, _ := offset.Div(span).Mul(decimal.NewFromInt(100)).Float64()
		rangePos = p
	}

	direction := candidate.RecommendedTrend
	if direction == "" || direction == "no_trend" {
		direction = "neutral"
	}

	// -------------------------------------------------------------
	// ANTI-FOMO TOP SHIELD CHECKS
	// -------------------------------------------------------------
	if direction == "long" && rangePos > 40.0 {
		eval.RejectionReasons = append(eval.RejectionReasons, "Anti-FOMO: цена выше 40% диапазона (вход на хаях запрещен)")
	}
	if direction == "short" && rangePos < 60.0 {
		eval.RejectionReasons = append(eval.RejectionReasons, "Anti-FOMO: цена ниже 60% диапазона (шорт на дне запрещен)")
	}

	// Check Funding Overheat
	if fundingRate != nil && direction == "long" {
		if fr, _ := fundingRate.Float64(); fr > 0.0005 { // > +0.05%
			eval.RejectionReasons = append(eval.RejectionReasons, "Anti-FOMO: ставка фандинга перегрета (> +0.05%), риск лонг-сквиза")
		}
	}

	// -------------------------------------------------------------
	// FACTOR 1: CHANNEL LEVEL & FIBONACCI GOLDEN POCKET (Max 35 pts)
	// -------------------------------------------------------------
	switch direction {
	case "long":
		// Ideal: 10% to 30% channel support
		if rangePos >= 10.0 && rangePos <= 30.0 {
			eval.Factor1LevelScore = 35.0
			eval.TargetEntryPrice = candidate.CurrentPrice
		} else if rangePos > 30.0 && rangePos <= 45.0 {
			eval.Factor1LevelScore = 20.0
			// Target entry is at 20% of channel
			span := candidate.UpperPrice.Sub(candidate.LowerPrice)
			eval.TargetEntryPrice = candidate.LowerPrice.Add(span.Mul(decimal.NewFromFloat(0.20)))
		} else {
			eval.Factor1LevelScore = 5.0
			span := candidate.UpperPrice.Sub(candidate.LowerPrice)
			eval.TargetEntryPrice = candidate.LowerPrice.Add(span.Mul(decimal.NewFromFloat(0.20)))
		}
	case "short":
		// Ideal: 70% to 90% channel resistance
		if rangePos >= 70.0 && rangePos <= 90.0 {
			eval.Factor1LevelScore = 35.0
			eval.TargetEntryPrice = candidate.CurrentPrice
		} else if rangePos >= 55.0 && rangePos < 70.0 {
			eval.Factor1LevelScore = 20.0
			span := candidate.UpperPrice.Sub(candidate.LowerPrice)
			eval.TargetEntryPrice = candidate.LowerPrice.Add(span.Mul(decimal.NewFromFloat(0.80)))
		} else {
			eval.Factor1LevelScore = 5.0
			span := candidate.UpperPrice.Sub(candidate.LowerPrice)
			eval.TargetEntryPrice = candidate.LowerPrice.Add(span.Mul(decimal.NewFromFloat(0.80)))
		}
	default:
		// Neutral: 30% to 70% channel core
		if rangePos >= 30.0 && rangePos <= 70.0 {
			eval.Factor1LevelScore = 35.0
			eval.TargetEntryPrice = candidate.CurrentPrice
		} else {
			eval.Factor1LevelScore = 15.0
			eval.TargetEntryPrice = candidate.LowerPrice.Add(candidate.UpperPrice.Sub(candidate.LowerPrice).Mul(decimal.NewFromFloat(0.50)))
		}
	}

	// -------------------------------------------------------------
	// FACTOR 2: MOMENTUM & MFI / RSI DIVERGENCE (Max 35 pts)
	// -------------------------------------------------------------
	var adx float64 = 20.0
	if v, ok := candidate.ModelAssumptions["adx"].(float64); ok {
		adx = v
	}

	// ADX filter: Trending killers (> 35) are penalized
	if adx < 25.0 {
		eval.Factor2MomentumScore += 20.0 // Healthy consolidation
	} else if adx <= 35.0 {
		eval.Factor2MomentumScore += 10.0
	} else {
		eval.RejectionReasons = append(eval.RejectionReasons, "ADX > 35: слишком сильный тренд для сетки")
	}

	// RSI / Choppiness proxy
	var chop float64 = 50.0
	if v, ok := candidate.ModelAssumptions["choppiness"].(float64); ok {
		chop = v
	}
	if chop >= 50.0 {
		eval.Factor2MomentumScore += 15.0 // Strong mean-reversion
	} else if chop >= 40.0 {
		eval.Factor2MomentumScore += 10.0
	}

	// -------------------------------------------------------------
	// FACTOR 3: CANDLE PHYSICS & REVERSAL TRIGGER (Max 30 pts)
	// -------------------------------------------------------------
	if len(candles) >= 2 {
		lastCandle := candles[len(candles)-1]
		open, _ := lastCandle.Open.Float64()
		close, _ := lastCandle.Close.Float64()
		high, _ := lastCandle.High.Float64()
		low, _ := lastCandle.Low.Float64()
		totalSpan := high - low

		if totalSpan > 0 {
			upperWick := high - math.Max(open, close)
			lowerWick := math.Min(open, close) - low
			lowerWickRatio := lowerWick / totalSpan
			upperWickRatio := upperWick / totalSpan

			if direction == "long" {
				if upperWickRatio >= 0.40 {
					eval.RejectionReasons = append(eval.RejectionReasons, "Anti-FOMO: верхняя тень свечи >= 40% (сброс продавца на хае)")
				}
				if lowerWickRatio >= 0.45 {
					// Bullish Pin-bar / Hammer confirmed!
					eval.Factor3CandleScore += 30.0
				} else if close >= open {
					// Bullish green close
					eval.Factor3CandleScore += 20.0
				} else {
					eval.Factor3CandleScore += 10.0
				}
			} else if direction == "short" {
				if lowerWickRatio >= 0.40 {
					eval.RejectionReasons = append(eval.RejectionReasons, "Anti-FOMO: нижняя тень свечи >= 40% (откуп на лое)")
				}
				if upperWickRatio >= 0.45 {
					// Bearish Shooting Star confirmed!
					eval.Factor3CandleScore += 30.0
				} else if close <= open {
					eval.Factor3CandleScore += 20.0
				} else {
					eval.Factor3CandleScore += 10.0
				}
			} else {
				// Neutral: symmetric wicks
				eval.Factor3CandleScore += 25.0
			}
		} else {
			eval.Factor3CandleScore += 20.0
		}
	} else {
		eval.Factor3CandleScore += 20.0
	}

	eval.Score = eval.Factor1LevelScore + eval.Factor2MomentumScore + eval.Factor3CandleScore
	if eval.TargetEntryPrice.GreaterThan(decimal.Zero) && candidate.CurrentPrice.GreaterThan(decimal.Zero) {
		diff := candidate.CurrentPrice.Sub(eval.TargetEntryPrice)
		d, _ := diff.Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
		eval.DistanceToEntryPct = math.Round(d*100) / 100
	}

	if len(eval.RejectionReasons) > 0 {
		eval.IsFired = false
		eval.Status = "OVERHEATED"
	} else if eval.Score >= 75.0 {
		eval.IsFired = true
		eval.Status = "TRIGGERED"
	} else if eval.Score >= 50.0 {
		eval.IsFired = false
		eval.Status = "ARMED"
	} else {
		eval.IsFired = false
		eval.Status = "WAITING"
	}

	return eval
}

// IsSqueezeAccumulating returns true if the asset is in a tight volatility squeeze (accumulation phase).
func IsSqueezeAccumulating(bandwidthPct float64, choppiness float64) bool {
	return bandwidthPct < 3.5 && choppiness > 55.0
}
