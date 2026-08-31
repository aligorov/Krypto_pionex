-- Lifecycle telemetry (v2.0.54): the analytics wall of 2026-08-31 was that
-- behavior existed only as scalars (peak/trough) and events. This adds:
--  1. bot_telemetry — one row per running bot per manage pass (the
--     underwater-duration / recovery-shape trace);
--  2. paper_grid_bots.pairs_completed + funding_paid_usdt — grid activity
--     and net carry per bot (the harvest vs bleed split);
--  3. autogrid_candidates.outcome_* — entry features and final outcome in
--     ONE table: the score-calibration query becomes trivial and survives
--     candidate turnover.

CREATE TABLE IF NOT EXISTS bot_telemetry (
    id BIGSERIAL PRIMARY KEY,
    bot_id UUID NOT NULL,
    bot_number INT NOT NULL DEFAULT 0,
    symbol TEXT NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    price NUMERIC(20,10) NOT NULL,
    realized_pnl NUMERIC(14,6) NOT NULL DEFAULT 0,
    unrealized_pnl NUMERIC(14,6) NOT NULL DEFAULT 0,
    total_pnl NUMERIC(14,6) NOT NULL DEFAULT 0,
    grid_level INT NOT NULL DEFAULT 0,
    inventory_notional NUMERIC(14,6) NOT NULL DEFAULT 0,
    adjustments_count INT NOT NULL DEFAULT 0,
    funding_paid_usdt NUMERIC(14,6) NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_bot_telemetry_bot_time
    ON bot_telemetry (bot_id, captured_at DESC);
CREATE INDEX IF NOT EXISTS idx_bot_telemetry_time
    ON bot_telemetry (captured_at);

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS pairs_completed INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS funding_paid_usdt NUMERIC(14,6) NOT NULL DEFAULT 0;

ALTER TABLE autogrid_candidates
    ADD COLUMN IF NOT EXISTS outcome_pnl_usdt NUMERIC(14,6),
    ADD COLUMN IF NOT EXISTS outcome_closed_reason TEXT,
    ADD COLUMN IF NOT EXISTS outcome_at TIMESTAMPTZ;
