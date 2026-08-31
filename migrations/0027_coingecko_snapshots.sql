-- Migration 0027: CoinGecko macro snapshots (free, key-less public API).
-- Purpose: independent BTC price/24h context and BTC-dominance history for
-- the beta/alt-drain entry vetoes. BTC-dominance delta over 24h is computed
-- from this table because CoinGecko /global exposes no history on the free
-- tier.

CREATE TABLE IF NOT EXISTS coingecko_snapshots (
    id BIGSERIAL PRIMARY KEY,
    btc_usd NUMERIC(18,4) NOT NULL,
    btc_24h_pct NUMERIC(10,6) NOT NULL,
    btc_dominance_pct NUMERIC(10,6),
    total_mcap_usd NUMERIC(24,4),
    mcap_24h_pct NUMERIC(10,6),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_coingecko_captured_at ON coingecko_snapshots(captured_at DESC);
