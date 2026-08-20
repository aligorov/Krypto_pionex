package autogrid

import (
	"context"
	"fmt"
	"math"
)

// CheckEconomicEvents queries the economic_events table for high-impact
// events around NOW: blocked window is [event − hoursAhead, event + 1 hour].
// Since v2.0.8 the lookahead is 2h (was 12h — with 2-4 USD High events a
// week that froze deployments 15-30% of wall clock) and the block now covers
// the release itself plus the first hour after — previously it lifted at the
// exact minute the print dropped, i.e. right into peak volatility. Only USD
// macro (FOMC, Fed decisions, US CPI/NFP) blocks entries; non-USD releases
// stay informational (v2.07).
func (worker *Worker) CheckEconomicEvents(ctx context.Context, hoursAhead int) (bool, string) {
	const whereClause = `
        WHERE impact = 'High'
          AND (country = 'USD' OR country IS NULL OR country = '')
          AND event_time BETWEEN NOW() - INTERVAL '1 hour' AND NOW() + ($1 || ' hours')::interval
    `
	var count int
	err := worker.db.QueryRow(ctx, `
        SELECT COUNT(*) FROM economic_events`+whereClause,
		fmt.Sprintf("%d", hoursAhead)).Scan(&count)
	if err != nil || count == 0 {
		return false, ""
	}

	// Get the event title
	var title string
	_ = worker.db.QueryRow(ctx, `
        SELECT title FROM economic_events`+whereClause+`
        ORDER BY ABS(EXTRACT(EPOCH FROM (event_time - NOW()))) LIMIT 1`,
		fmt.Sprintf("%d", hoursAhead)).Scan(&title)

	return true, title
}

// CheckLiquidationCascade checks if there were major liquidations recently.
// Reads from the liquidation_events table (populated by the collector).
// CheckLiquidationCascade arms on LONG liquidations only (v2.0.14): forced
// long unwinding is the falling-knife signature this gate exists for. A
// short-squeeze cascade (side='short') marks a violent UP move — precisely
// when participation finally makes sense — and must not freeze entries.
func (worker *Worker) CheckLiquidationCascade(ctx context.Context, thresholdUSD float64) (bool, float64) {
	var totalUSD float64
	err := worker.db.QueryRow(ctx, `
        SELECT COALESCE(SUM(value_usd), 0) FROM liquidation_events
        WHERE captured_at > NOW() - INTERVAL '1 hour'
          AND side = 'long'
    `).Scan(&totalUSD)
	if err != nil {
		return false, 0
	}
	return totalUSD > thresholdUSD, totalUSD
}

// GetFundingForSymbol gets the latest cross-exchange funding rate.
func (worker *Worker) GetFundingForSymbol(ctx context.Context, symbol string) (*FundingContext, error) {
	var avgRate float64
	var count int
	err := worker.db.QueryRow(ctx, `
        SELECT AVG(funding_rate), COUNT(*)
        FROM funding_snapshots
        WHERE symbol = $1 AND captured_at > NOW() - INTERVAL '10 minutes'
    `, symbol).Scan(&avgRate, &count)
	if err != nil || count == 0 {
		return nil, fmt.Errorf("no recent funding data for %s", symbol)
	}

	return &FundingContext{
		AverageRate: avgRate,
		IsExtreme:   math.Abs(avgRate) > 0.001,
	}, nil
}

// GetFearGreed returns the latest Fear & Greed Index value.
func (worker *Worker) GetFearGreed(ctx context.Context) (int, error) {
	var value float64
	err := worker.db.QueryRow(ctx, `
        SELECT value FROM sentiment_snapshots
        WHERE source = 'fng'
          AND captured_at > NOW() - INTERVAL '36 hours'
        ORDER BY captured_at DESC LIMIT 1
    `).Scan(&value)
	if err != nil {
		// Neutral default both on error and on staleness: a frozen euphoria
		// (>=85) or panic (1..15) reading must not freeze entries fleet-wide
		// for as long as the feed is dead (2026-08-20 audit).
		return 50, nil
	}
	return int(value), nil
}
