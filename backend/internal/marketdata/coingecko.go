package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// CoinGecko free public API (no key): independent BTC spot context and BTC
// dominance for the macro entry vetoes. This is market CONTEXT only — every
// trading decision still prices and executes against native Pionex data.
// The free tier exposes no historical /global, so dominance deltas over 24h
// are computed from our own snapshot history in coingecko_snapshots.

const (
	DefaultCoinGeckoBaseURL = "https://api.coingecko.com"
	limiterKeyCoinGecko     = "coingecko"
)

var errCoinGeckoEmpty = errors.New("coingecko: empty price payload")

type coinGeckoSimplePrice struct {
	USD          float64 `json:"usd"`
	USD24hChange float64 `json:"usd_24h_change"`
}

type coinGeckoSimpleResponse struct {
	Bitcoin coinGeckoSimplePrice `json:"bitcoin"`
}

type coinGeckoGlobalResponse struct {
	Data struct {
		MarketCapPercentage map[string]float64 `json:"market_cap_percentage"`
		TotalMarketCap      map[string]float64 `json:"total_market_cap"`
		MCapChange24hPct    float64            `json:"market_cap_change_percentage_24h_usd"`
	} `json:"data"`
}

// CoinGeckoSnapshot is one persisted macro sample. Zero dominance means the
// advisory /global call failed and only price context is available.
type CoinGeckoSnapshot struct {
	BTCUSD          float64
	BTC24hPct       float64
	BTCDominancePct float64
	TotalMCapUSD    float64
	MCap24hPct      float64
	CapturedAt      time.Time
}

func (c *Collector) fetchCoinGecko(ctx context.Context) (*CoinGeckoSnapshot, error) {
	var simple coinGeckoSimpleResponse
	simpleURL := c.cfg.CoinGeckoBaseURL + "/api/v3/simple/price?" + url.Values{
		"ids": {"bitcoin"}, "vs_currencies": {"usd"}, "include_24hr_change": {"true"},
	}.Encode()
	if err := c.getJSON(ctx, limiterKeyCoinGecko, simpleURL, &simple); err != nil {
		return nil, err
	}
	if simple.Bitcoin.USD <= 0 {
		return nil, errCoinGeckoEmpty
	}
	snap := &CoinGeckoSnapshot{
		BTCUSD:    simple.Bitcoin.USD,
		BTC24hPct: simple.Bitcoin.USD24hChange,
	}
	// /global is advisory: a failure degrades to price-only snapshots.
	var global coinGeckoGlobalResponse
	globalURL := c.cfg.CoinGeckoBaseURL + "/api/v3/global"
	if err := c.getJSON(ctx, limiterKeyCoinGecko, globalURL, &global); err == nil {
		if dom, ok := global.Data.MarketCapPercentage["btc"]; ok {
			snap.BTCDominancePct = dom
		}
		if mcap, ok := global.Data.TotalMarketCap["usd"]; ok {
			snap.TotalMCapUSD = mcap
		}
		snap.MCap24hPct = global.Data.MCapChange24hPct
	}
	snap.CapturedAt = time.Now().UTC()
	return snap, nil
}

// collectCoinGecko persists one snapshot every interval (default 10m — well
// inside the free tier limits) and prunes history older than a week.
func (c *Collector) collectCoinGecko(ctx context.Context) {
	snap, err := c.fetchCoinGecko(ctx)
	if err != nil {
		slog.Warn("coingecko collector: fetch failed", "error", err)
		return
	}
	if _, err := c.db.Exec(ctx, `
		INSERT INTO coingecko_snapshots
			(btc_usd, btc_24h_pct, btc_dominance_pct, total_mcap_usd, mcap_24h_pct)
		VALUES ($1, $2, NULLIF($3, 0), NULLIF($4, 0), $5)
	`,
		decimal.NewFromFloat(snap.BTCUSD).Round(4),
		decimal.NewFromFloat(snap.BTC24hPct).Round(6),
		decimal.NewFromFloat(snap.BTCDominancePct).Round(6),
		decimal.NewFromFloat(snap.TotalMCapUSD).Round(4),
		decimal.NewFromFloat(snap.MCap24hPct).Round(6),
	); err != nil {
		slog.Warn("coingecko collector: persist failed", "error", err)
		return
	}
	if _, err := c.db.Exec(ctx, `
		DELETE FROM coingecko_snapshots WHERE captured_at < NOW() - INTERVAL '7 days'
	`); err != nil {
		slog.Warn("coingecko collector: prune failed", "error", err)
	}
}

// LatestCoinGeckoWindow returns the newest snapshot plus the newest snapshot
// at least minAge old (the ~24h anchor for dominance deltas). aged is nil
// while history is still accumulating — callers must treat that as "gate
// not yet armed", never as a veto.
func LatestCoinGeckoWindow(ctx context.Context, db *pgxpool.Pool, minAge time.Duration) (latest, aged *CoinGeckoSnapshot, err error) {
	var l, a CoinGeckoSnapshot
	var domL, domA decimal.Decimal
	err = db.QueryRow(ctx, `
		SELECT btc_usd, btc_24h_pct, COALESCE(btc_dominance_pct, 0), captured_at
		FROM coingecko_snapshots
		ORDER BY captured_at DESC LIMIT 1
	`).Scan(&l.BTCUSD, &l.BTC24hPct, &domL, &l.CapturedAt)
	if err != nil {
		return nil, nil, err
	}
	l.BTCDominancePct, _ = domL.Float64()
	latest = &l

	err = db.QueryRow(ctx, `
		SELECT btc_usd, btc_24h_pct, COALESCE(btc_dominance_pct, 0), captured_at
		FROM coingecko_snapshots
		WHERE captured_at <= NOW() - $1::interval
		ORDER BY captured_at DESC LIMIT 1
	`, minAge.String()).Scan(&a.BTCUSD, &a.BTC24hPct, &domA, &a.CapturedAt)
	if err != nil {
		return latest, nil, nil
	}
	a.BTCDominancePct, _ = domA.Float64()
	return latest, &a, nil
}
