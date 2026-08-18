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

// LiquidationCascadeThresholdUSD matches the autogrid worker gate: totals
// above this within one hour read as a liquidation cascade.
const LiquidationCascadeThresholdUSD = 50_000_000

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

// LiquidationSummary aggregates the trailing hour of liquidation events.
// An empty table yields a zero summary with Cascade=false.
type LiquidationSummary struct {
	Total1hUSD float64 `json:"total1hUSD"`
	Cascade    bool    `json:"cascade"` // total1hUSD > LiquidationCascadeThresholdUSD
}

// FearGreedSnapshot is the latest Fear & Greed reading (0-100) with its
// human classification ("Fear", "Greed", ...).
type FearGreedSnapshot struct {
	Value          float64 `json:"value"`
	Classification string  `json:"classification"`
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

// GetCurrentFundingBatch is the multi-symbol variant of GetCurrentFunding in
// a single round-trip. Symbols without snapshots inside the 10-minute window
// are simply absent from the result map — callers treat that as "no data".
func (s *Service) GetCurrentFundingBatch(ctx context.Context, symbols []string) (map[string]*FundingInfo, error) {
	if len(symbols) == 0 {
		return map[string]*FundingInfo{}, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT symbol, exchange, funding_rate
		FROM (
			SELECT DISTINCT ON (symbol, exchange) symbol, exchange, funding_rate
			FROM funding_snapshots
			WHERE symbol = ANY($1) AND captured_at >= NOW() - INTERVAL '10 minutes'
			ORDER BY symbol, exchange, captured_at DESC
		) latest
	`, symbols)
	if err != nil {
		return nil, fmt.Errorf("query funding snapshots batch: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*FundingInfo, len(symbols))
	reported := make(map[string]int, len(symbols))
	for rows.Next() {
		var symbol, exchange string
		var rate float64
		if err := rows.Scan(&symbol, &exchange, &rate); err != nil {
			return nil, fmt.Errorf("scan funding snapshot batch: %w", err)
		}
		info := result[symbol]
		if info == nil {
			info = &FundingInfo{}
			result[symbol] = info
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
		reported[symbol]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate funding snapshots batch: %w", err)
	}
	for symbol, info := range result {
		info.AverageRate /= float64(reported[symbol])
		info.IsExtreme = FundingIsExtreme(info.AverageRate)
	}
	return result, nil
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

// GetFearGreed returns the latest Fear & Greed index snapshot (0-100 plus
// classification). When the stored classification is NULL it is derived from
// the standard alternative.me bands.
func (s *Service) GetFearGreed(ctx context.Context) (*FearGreedSnapshot, error) {
	var value *float64
	var classification *string
	if err := s.db.QueryRow(ctx, `
		SELECT value, classification
		FROM sentiment_snapshots
		WHERE source = 'fng'
		ORDER BY captured_at DESC
		LIMIT 1
	`).Scan(&value, &classification); err != nil {
		return nil, fmt.Errorf("query latest fear & greed: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("latest fear & greed value is NULL")
	}
	snapshot := &FearGreedSnapshot{Value: *value}
	if classification != nil && *classification != "" {
		snapshot.Classification = *classification
	} else {
		snapshot.Classification = classifyFearGreed(snapshot.Value)
	}
	return snapshot, nil
}

// classifyFearGreed maps a 0-100 index value to the standard band names.
func classifyFearGreed(value float64) string {
	switch {
	case value < 25:
		return "Extreme Fear"
	case value < 45:
		return "Fear"
	case value <= 55:
		return "Neutral"
	case value <= 75:
		return "Greed"
	default:
		return "Extreme Greed"
	}
}

// GetLiquidationSummary sums liquidation events (USD) over the trailing
// hour across all symbols. An empty table (or no recent rows) returns a
// zero summary with Cascade=false rather than an error.
func (s *Service) GetLiquidationSummary(ctx context.Context) (*LiquidationSummary, error) {
	var total float64
	if err := s.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(value_usd), 0)
		FROM liquidation_events
		WHERE captured_at > NOW() - INTERVAL '1 hour'
	`).Scan(&total); err != nil {
		return nil, fmt.Errorf("sum liquidation events: %w", err)
	}
	return &LiquidationSummary{
		Total1hUSD: total,
		Cascade:    total > LiquidationCascadeThresholdUSD,
	}, nil
}
