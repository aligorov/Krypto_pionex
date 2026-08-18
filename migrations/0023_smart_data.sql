-- Migration 0023: smart data pipeline for the v2.0 Smart Grid Engine.
-- Numbered 0023 because 0022 was already taken by 0022_paper_funding.sql;
-- the migration runner uses the file name as the version, so prefixes must
-- stay unique.

-- Funding snapshots from multiple exchanges
CREATE TABLE IF NOT EXISTS funding_snapshots (
    id BIGSERIAL PRIMARY KEY,
    symbol VARCHAR(32) NOT NULL,
    exchange VARCHAR(16) NOT NULL,
    funding_rate NUMERIC(12,10) NOT NULL,
    mark_price NUMERIC(30,10),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_funding_symbol_time ON funding_snapshots(symbol, captured_at DESC);

-- Open Interest history
CREATE TABLE IF NOT EXISTS oi_history (
    id BIGSERIAL PRIMARY KEY,
    symbol VARCHAR(32) NOT NULL,
    exchange VARCHAR(16) NOT NULL,
    oi_usd NUMERIC(30,10),
    price NUMERIC(30,10),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_oi_symbol_time ON oi_history(symbol, captured_at DESC);

-- Sentiment (Fear & Greed)
CREATE TABLE IF NOT EXISTS sentiment_snapshots (
    id SERIAL PRIMARY KEY,
    source VARCHAR(16) NOT NULL DEFAULT 'fng',
    value NUMERIC(8,2),
    classification VARCHAR(24),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Economic events (FOMC, CPI)
CREATE TABLE IF NOT EXISTS economic_events (
    id SERIAL PRIMARY KEY,
    title VARCHAR(255) NOT NULL,
    event_time TIMESTAMPTZ NOT NULL,
    impact VARCHAR(8) NOT NULL,
    country VARCHAR(8),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
