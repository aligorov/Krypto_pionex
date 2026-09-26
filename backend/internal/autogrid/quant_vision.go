package autogrid

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

// evaluateWickShield checks whether a stop-loss, structural invalidation, or range-break close
// was triggered by an intraday liquidity sweep wick (SFP / long lower wick for LONG/NEUTRAL,
// or long upper wick for SHORT). If a rejection wick is detected, the stop is deferred
// for up to graceSec seconds to prevent getting stopped out at the exact bottom/top of the wick.
func (worker *Worker) evaluateWickShield(
	ctx context.Context,
	symbol string,
	direction string,
	currentPrice decimal.Decimal,
	triggeredAt *string,
	extreme *decimal.Decimal,
	graceSec int,
) (shouldHold bool, newTriggeredAt *string, newExtreme *decimal.Decimal, reason string) {
	if graceSec <= 0 {
		graceSec = 90
	}

	// Case 1: Wick shield is already armed on this bot
	if triggeredAt != nil && strings.TrimSpace(*triggeredAt) != "" {
		stamped, err := time.Parse(time.RFC3339, strings.TrimSpace(*triggeredAt))
		if err != nil {
			return false, nil, nil, "invalid wick shield timestamp"
		}
		if extreme == nil || extreme.IsZero() {
			return false, nil, nil, "missing wick shield extreme price"
		}

		// Check if extreme was violated
		if strings.ToUpper(direction) == "SHORT" {
			// For SHORT, breach means price punched HIGHER than the wick high
			if currentPrice.GreaterThan(*extreme) {
				return false, nil, nil, fmt.Sprintf("wick shield breached: price %s > wick high %s", currentPrice.String(), extreme.String())
			}
		} else {
			// For LONG and NEUTRAL, breach means price punched LOWER than the wick low
			if currentPrice.LessThan(*extreme) {
				return false, nil, nil, fmt.Sprintf("wick shield breached: price %s < wick low %s", currentPrice.String(), extreme.String())
			}
		}

		elapsed := time.Since(stamped)
		if elapsed < time.Duration(graceSec)*time.Second {
			remaining := graceSec - int(elapsed.Seconds())
			return true, triggeredAt, extreme, fmt.Sprintf("wick shield active: holding (extreme %s, %ds left in grace window)", extreme.String(), remaining)
		}

		// Grace window expired and price hasn't recovered
		return false, nil, nil, fmt.Sprintf("wick shield grace window expired (%ds passed)", int(elapsed.Seconds()))
	}

	// Case 2: Shield not yet armed. Inspect latest 5M candle for a rejection wick.
	if worker.publicClient == nil {
		return false, nil, nil, "public client not initialized"
	}

	// Fetch 2 candles of 5M interval
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "5M", 2)
	if err != nil || len(candles) == 0 {
		return false, nil, nil, "failed to fetch 5M candles for wick analysis"
	}

	latestCandle := candles[len(candles)-1]
	analysis := marketdata.AnalyzeCandle(latestCandle)

	dir := strings.ToUpper(direction)
	if dir == "SHORT" {
		// Bearish wick: Upper wick is long, price is bouncing downward
		if (analysis.UpperWickRatio >= 0.40 || analysis.IsPinBarBear) &&
			currentPrice.LessThan(latestCandle.High) {
			nowStr := time.Now().UTC().Format(time.RFC3339)
			high := latestCandle.High
			return true, &nowStr, &high, fmt.Sprintf("armed wick shield: 5m upper wick ratio %.2f (high %s)", analysis.UpperWickRatio, high.String())
		}
	} else {
		// Bullish wick: Lower wick is long, price is bouncing upward
		if (analysis.LowerWickRatio >= 0.40 || analysis.IsPinBarBull) &&
			currentPrice.GreaterThan(latestCandle.Low) {
			nowStr := time.Now().UTC().Format(time.RFC3339)
			low := latestCandle.Low
			return true, &nowStr, &low, fmt.Sprintf("armed wick shield: 5m lower wick ratio %.2f (low %s)", analysis.LowerWickRatio, low.String())
		}
	}

	return false, nil, nil, "no rejection wick formed"
}

// directionalConfirmedByPriceAction checks if recent candlestick price action
// (Pin Bar, Engulfing, or SFP liquidity sweep) confirms a directional entry thesis (LONG or SHORT) on 15M candles.
func (worker *Worker) directionalConfirmedByPriceAction(ctx context.Context, symbol string, trend string) (bool, string) {
	if worker.publicClient == nil {
		return false, "no public client"
	}
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "15M", 3)
	if err != nil || len(candles) < 2 {
		return false, "insufficient candle data"
	}
	prev := candles[len(candles)-2]
	curr := candles[len(candles)-1]
	currAnalysis := marketdata.AnalyzeCandle(curr)

	t := strings.ToLower(trend)
	if t == "long" {
		if currAnalysis.IsPinBarBull {
			return true, fmt.Sprintf("Bullish Pin Bar / Hammer (lower wick %.0f%%)", currAnalysis.LowerWickRatio*100)
		}
		if isEngulfing, side := marketdata.DetectEngulfing(prev, curr); isEngulfing && side == "BULLISH" {
			return true, "Bullish Engulfing candle"
		}
		lookbackLow := prev.Low
		if isSweep, wickRatio := marketdata.DetectSFP(lookbackLow, curr); isSweep {
			return true, fmt.Sprintf("Bullish SFP sweep (swept %s, wick %.0f%%, closed %s)", lookbackLow.String(), wickRatio*100, curr.Close.String())
		}
	} else if t == "short" {
		if currAnalysis.IsPinBarBear {
			return true, fmt.Sprintf("Bearish Pin Bar / Shooting Star (upper wick %.0f%%)", currAnalysis.UpperWickRatio*100)
		}
		if isEngulfing, side := marketdata.DetectEngulfing(prev, curr); isEngulfing && side == "BEARISH" {
			return true, "Bearish Engulfing candle"
		}
	}
	return false, "no confirming candlestick pattern"
}

// calculateFleetNetDelta sums the total directional delta (USDT notional) across
// all active running grid bots to prevent over-concentrated directional exposure.
func (worker *Worker) calculateFleetNetDelta(ctx context.Context, settingsID string, paper bool) (decimal.Decimal, error) {
	tableName := "grid_bots"
	colSettings := "autogrid_settings_id"
	extraFilter := "AND bu_order_id IS NOT NULL"
	if paper {
		tableName = "paper_grid_bots"
		colSettings = "settings_id"
		extraFilter = ""
	}

	query := fmt.Sprintf(`
		SELECT direction, quote_investment, leverage,
		       COALESCE(NULLIF(model_state->>'shiftPosition','')::NUMERIC, 0),
		       COALESCE(mark_price, entry_price, 0)
		FROM %s
		WHERE %s = $1 AND status = 'RUNNING' %s
	`, tableName, colSettings, extraFilter)

	rows, err := worker.db.Query(ctx, query, settingsID)
	if err != nil {
		return decimal.Zero, err
	}
	defer rows.Close()

	totalDelta := decimal.Zero
	for rows.Next() {
		var direction string
		var investment, shiftPos, markPrice decimal.Decimal
		var leverage int
		if err := rows.Scan(&direction, &investment, &leverage, &shiftPos, &markPrice); err != nil {
			continue
		}

		if leverage <= 0 {
			leverage = 1
		}

		var botDelta decimal.Decimal
		if !shiftPos.IsZero() && markPrice.IsPositive() {
			botDelta = shiftPos.Mul(markPrice)
		} else {
			dir := strings.ToUpper(direction)
			if dir == "LONG" {
				botDelta = investment.Mul(decimal.NewFromInt(int64(leverage)))
			} else if dir == "SHORT" {
				botDelta = investment.Mul(decimal.NewFromInt(int64(leverage))).Neg()
			}
		}
		totalDelta = totalDelta.Add(botDelta)
	}

	return totalDelta, nil
}

// checkOrderBookCushion queries the L2 depth and evaluates whether the order book
// provides adequate liquidity cushion to absorb orders without slippage cliffs.
func (worker *Worker) checkOrderBookCushion(
	ctx context.Context,
	symbol string,
	currentPrice decimal.Decimal,
	botNotional float64,
	minCushionRatio float64,
) (ok bool, profile marketdata.DepthProfile, reason string) {
	if worker.publicClient == nil {
		return true, marketdata.DepthProfile{}, "no public client"
	}
	bids, asks, err := worker.publicClient.GetDepth(ctx, symbol, 50)
	if err != nil {
		return true, marketdata.DepthProfile{}, "depth fetch failed (fail-open)"
	}
	profile = marketdata.ProfileOrderBook(bids, asks, currentPrice, botNotional, minCushionRatio)
	if profile.IsThinBook {
		return false, profile, fmt.Sprintf("глубина 2%% стакана $%.0f составляет %.1fx от размера бота (нужно ≥%.0fx)",
			profile.BidVolumeUSDT, profile.BidCushionRatio, minCushionRatio)
	}
	return true, profile, ""
}

// checkKnifePause checks recent market trades. If aggressive market taker dumping
// (≥75% sell volume) is occurring, LONG and NEUTRAL entries are paused to avoid catching a falling knife.
func (worker *Worker) checkKnifePause(
	ctx context.Context,
	symbol string,
	trend string,
) (paused bool, metrics marketdata.TakerFlowMetrics, reason string) {
	if worker.publicClient == nil {
		return false, marketdata.TakerFlowMetrics{}, "no public client"
	}
	trades, err := worker.publicClient.GetTrades(ctx, symbol, 60)
	if err != nil || len(trades) == 0 {
		return false, marketdata.TakerFlowMetrics{}, "trades fetch failed (fail-open)"
	}
	metrics = marketdata.AnalyzeTakerFlow(trades)
	t := strings.ToLower(trend)
	if (t == "long" || t == "neutral" || t == "no_trend") && metrics.IsAggressiveDumping {
		return true, metrics, fmt.Sprintf("агрессивный сброс маркет-ордерами: %.0f%% taker sell volume ($%.0f) — нож падает",
			metrics.SellRatio*100, metrics.SellVolumeUSDT)
	}
	if t == "short" && metrics.IsAggressivePumping {
		return true, metrics, fmt.Sprintf("агрессивный памп маркет-ордерами: %.0f%% taker buy volume ($%.0f) — ракета вверх",
			metrics.BuyRatio*100, metrics.BuyVolumeUSDT)
	}
	return false, metrics, ""
}
