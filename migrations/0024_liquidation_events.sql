-- Migration 0024: liquidation events feed for the Smart Grid Engine v2.0.
-- Backs marketdata.GetLiquidationSummary and the autogrid liquidation gate
-- (both aggregate value_usd over the trailing hour).

CREATE TABLE IF NOT EXISTS liquidation_events (
    id BIGSERIAL PRIMARY KEY,
    symbol VARCHAR(32) NOT NULL,
    side VARCHAR(8),                 -- 'long' | 'short' liquidated side
    value_usd NUMERIC(30,10) NOT NULL DEFAULT 0,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_liquidation_time ON liquidation_events(captured_at DESC);
