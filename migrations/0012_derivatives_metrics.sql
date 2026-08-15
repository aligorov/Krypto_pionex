-- Migration 0012: Real-time Derivatives Intelligence (Funding Rate & Open Interest)

CREATE TABLE IF NOT EXISTS market_derivatives_metrics (
    symbol VARCHAR(64) PRIMARY KEY,
    funding_rate NUMERIC(12, 8) NOT NULL DEFAULT 0,
    next_funding_time TIMESTAMPTZ,
    open_interest NUMERIC(24, 8) NOT NULL DEFAULT 0,
    open_interest_usd NUMERIC(24, 8) NOT NULL DEFAULT 0,
    volume_24h_usd NUMERIC(24, 8) NOT NULL DEFAULT 0,
    mark_price NUMERIC(24, 8) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_market_derivatives_funding ON market_derivatives_metrics (funding_rate);
CREATE INDEX IF NOT EXISTS idx_market_derivatives_oi ON market_derivatives_metrics (open_interest_usd DESC);
