package marketdata

import (
	"context"
	"fmt"
	"math"
	"time"
)

// ExtremeFundingThreshold is the absolute average funding rate (0.1% per 8h
// interval) above which funding conditions are considered extreme.
const ExtremeFundingThreshold = 0.001

// FundingInfo summarizes the latest cross-exchange funding conditions.
// Missing exchanges stay at zero; the average only includes exchanges that
// reported within the freshness window.
type FundingInfo struct {
	AverageRate float64 `json:"averageRate"` // average across reporting exchanges
	Binance     float64 `json:"binance"`
	Bybit       float64 `json:"bybit"`
	OKX         float64 `json:"okx"`
	IsExtreme   bool    `json:"isExtreme"` // |avg| > 0.001 (0.1%)
}

// OIInfo summarizes open interest changes over the trailing 24 hours.
type OIInfo struct {
	CurrentUSD float64 `json:"currentUsd"`
	ChangePct  float64 `json:"changePct"` // % change over 24h
	IsRising   bool    `json:"isRising"`
}

// EconomicEvent is a calendar entry with expected market impact.
type EconomicEvent struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	EventTime time.Time `json:"eventTime"`
	Impact    string    `json:"impact"`
	Country   string    `json:"country"`
}

// FundingIsExtreme reports whether an average funding rate exceeds the
// extreme threshold in absolute terms.
func FundingIsExtreme(averageRate float64) bool {
	return math.Abs(averageRate) > ExtremeFundingThreshold
}

// GetCurrentFunding returns the latest funding rate for a symbol, averaged
// across exchanges that reported within the last 10 minutes (the collector
// refreshes every 60s, so anything older is stale).
func (s *Service) GetCurrentFunding(ctx context.Context, symbol string) (*FundingInfo, error) {
	rows, err := s.db.Query(ctx, `
		SELECT exchange, funding_rate
		FROM (
			SELECT DISTINCT ON (exchange) exchange, funding_rate
			FROM funding_snapshots
			WHERE symbol = $1 AND captured_at >= NOW() - INTERVAL '10 minutes'
			ORDER BY exchange, captured_at DESC
		) latest
	`, symbol)
	if err != nil {
		return nil, fmt.Errorf("query funding snapshots for %s: %w", symbol, err)
	}
	defer rows.Close()

	info := &FundingInfo{}
	reported := 0
	for rows.Next() {
		var exchange string
		var rate float64
		if err := rows.Scan(&exchange, &rate); err != nil {
			return nil, fmt.Errorf("scan funding snapshot: %w", err)
		}
		switch exchange {
		case ExchangeBinance:
			info.Binance = rate
		case ExchangeBybit:
			info.Bybit = rate
		case ExchangeOKX:
			info.OKX = rate
		}
		info.AverageRate += rate
		reported++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate funding snapshots for %s: %w", symbol, err)
	}
	if reported == 0 {
		return nil, fmt.Errorf("no funding snapshots for %s within 10 minutes", symbol)
	}

	info.AverageRate /= float64(reported)
	info.IsExtreme = FundingIsExtreme(info.AverageRate)
	return info, nil
}

// GetOIChange24h returns the open interest change over the last 24 hours for
// a symbol. Per-exchange USD figures are aggregated (summed) before the
// percentage change is computed, so the result reflects total tracked OI.
func (s *Service) GetOIChange24h(ctx context.Context, symbol string) (*OIInfo, error) {
	rows, err := s.db.Query(ctx, `
		SELECT exchange,
		       (array_agg(oi_usd ORDER BY captured_at DESC))[1] AS latest_usd,
		       (array_agg(oi_usd ORDER BY captured_at ASC))[1]  AS oldest_usd
		FROM oi_history
		WHERE symbol = $1
		  AND captured_at >= NOW() - INTERVAL '24 hours'
		  AND oi_usd IS NOT NULL
		GROUP BY exchange
	`, symbol)
	if err != nil {
		return nil, fmt.Errorf("query oi history for %s: %w", symbol, err)
	}
	defer rows.Close()

	var latest, oldest float64
	exchanges := 0
	for rows.Next() {
		var exchange string
		var latestUSD, oldestUSD float64
		if err := rows.Scan(&exchange, &latestUSD, &oldestUSD); err != nil {
			return nil, fmt.Errorf("scan oi history: %w", err)
		}
		latest += latestUSD
		oldest += oldestUSD
		exchanges++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate oi history for %s: %w", symbol, err)
	}
	if exchanges == 0 {
		return nil, fmt.Errorf("no open interest history for %s within 24 hours", symbol)
	}

	info := &OIInfo{CurrentUSD: latest}
	if oldest > 0 {
		info.ChangePct = (latest - oldest) / oldest * 100
	}
	info.IsRising = info.ChangePct > 0
	return info, nil
}

// GetHighImpactEvents returns High impact events scheduled within the next
// hoursAhead hours, oldest first.
func (s *Service) GetHighImpactEvents(ctx context.Context, hoursAhead int) ([]EconomicEvent, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, title, event_time, impact, COALESCE(country, '')
		FROM economic_events
		WHERE impact = 'High'
		  AND event_time >= NOW()
		  AND event_time <= NOW() + ($1::int * INTERVAL '1 hour')
		ORDER BY event_time ASC
	`, hoursAhead)
	if err != nil {
		return nil, fmt.Errorf("query economic events: %w", err)
	}
	defer rows.Close()

	events := make([]EconomicEvent, 0)
	for rows.Next() {
		var event EconomicEvent
		if err := rows.Scan(&event.ID, &event.Title, &event.EventTime, &event.Impact, &event.Country); err != nil {
			return nil, fmt.Errorf("scan economic event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate economic events: %w", err)
	}
	return events, nil
}

// GetFearGreed returns the latest Fear & Greed index value (0-100).
func (s *Service) GetFearGreed(ctx context.Context) (float64, error) {
	var value *float64
	if err := s.db.QueryRow(ctx, `
		SELECT value
		FROM sentiment_snapshots
		WHERE source = 'fng'
		ORDER BY captured_at DESC
		LIMIT 1
	`).Scan(&value); err != nil {
		return 0, fmt.Errorf("query latest fear & greed: %w", err)
	}
	if value == nil {
		return 0, fmt.Errorf("latest fear & greed value is NULL")
	}
	return *value, nil
}
