package market

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type DerivativesMetrics struct {
	Symbol          string          `json:"symbol"`
	FundingRate     decimal.Decimal `json:"fundingRate"`
	NextFundingTime *time.Time      `json:"nextFundingTime"`
	OpenInterest    decimal.Decimal `json:"openInterest"`
	OpenInterestUSD decimal.Decimal `json:"openInterestUsd"`
	Volume24hUSD    decimal.Decimal `json:"volume24hUsd"`
	MarkPrice       decimal.Decimal `json:"markPrice"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

type DerivativesCollector struct {
	db     *pgxpool.Pool
	client *pionex.Client
	logger *slog.Logger
}

func NewDerivativesCollector(db *pgxpool.Pool, client *pionex.Client, logger *slog.Logger) *DerivativesCollector {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = pionex.NewClient("", "", "")
	}
	return &DerivativesCollector{
		db:     db,
		client: client,
		logger: logger,
	}
}

func (c *DerivativesCollector) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial run
	if err := c.Collect(ctx); err != nil {
		c.logger.Warn("initial derivatives collection failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Collect(ctx); err != nil {
				c.logger.Warn("derivatives collection failed", "err", err)
			}
		}
	}
}

func (c *DerivativesCollector) Collect(ctx context.Context) error {
	tickers, err := c.client.GetTickers(ctx, "", "PERP")
	if err != nil {
		tickers, err = c.client.GetTickers(ctx, "", "")
		if err != nil {
			return err
		}
	}

	for _, t := range tickers {
		if !strings.HasSuffix(t.Symbol, "_PERP") && !strings.Contains(t.Symbol, "PERP") {
			continue
		}
		volUsd := t.Amount
		markPrice := t.Close

		_, _ = c.db.Exec(ctx, `
			INSERT INTO market_derivatives_metrics (symbol, volume_24h_usd, mark_price, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (symbol) DO UPDATE SET
				volume_24h_usd = EXCLUDED.volume_24h_usd,
				mark_price = EXCLUDED.mark_price,
				updated_at = NOW()
		`, t.Symbol, volUsd, markPrice)
	}
	return nil
}

func (c *DerivativesCollector) GetMetrics(ctx context.Context, symbol string) (*DerivativesMetrics, error) {
	var m DerivativesMetrics
	row := c.db.QueryRow(ctx, `
		SELECT symbol, funding_rate, next_funding_time, open_interest, open_interest_usd, volume_24h_usd, mark_price, updated_at
		FROM market_derivatives_metrics
		WHERE symbol = $1
	`, symbol)
	if err := row.Scan(
		&m.Symbol,
		&m.FundingRate,
		&m.NextFundingTime,
		&m.OpenInterest,
		&m.OpenInterestUSD,
		&m.Volume24hUSD,
		&m.MarkPrice,
		&m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &m, nil
}
