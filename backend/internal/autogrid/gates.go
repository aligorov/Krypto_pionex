package autogrid

import (
	"context"
	"fmt"
	"math"
)

// CheckEconomicEvents queries the economic_events table for high-impact
// events in the next N hours.
func (worker *Worker) CheckEconomicEvents(ctx context.Context, hoursAhead int) (bool, string) {
	var count int
	err := worker.db.QueryRow(ctx, `
        SELECT COUNT(*) FROM economic_events
        WHERE impact = 'High'
          AND event_time BETWEEN NOW() AND NOW() + ($1 || ' hours')::interval
    `, fmt.Sprintf("%d", hoursAhead)).Scan(&count)
	if err != nil || count == 0 {
		return false, ""
	}

	// Get the event title
	var title string
	_ = worker.db.QueryRow(ctx, `
        SELECT title FROM economic_events
        WHERE impact = 'High'
          AND event_time BETWEEN NOW() AND NOW() + ($1 || ' hours')::interval
        ORDER BY event_time LIMIT 1
    `, fmt.Sprintf("%d", hoursAhead)).Scan(&title)

	return true, title
}

// CheckLiquidationCascade checks if there were major liquidations recently.
// Reads from the liquidation_events table (populated by the collector).
func (worker *Worker) CheckLiquidationCascade(ctx context.Context, thresholdUSD float64) (bool, float64) {
	var totalUSD float64
	err := worker.db.QueryRow(ctx, `
        SELECT COALESCE(SUM(value_usd), 0) FROM liquidation_events
        WHERE captured_at > NOW() - INTERVAL '1 hour'
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
        ORDER BY captured_at DESC LIMIT 1
    `).Scan(&value)
	if err != nil {
		return 50, nil // neutral default
	}
	return int(value), nil
}
