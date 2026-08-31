-- Migration 0029: stop-radar (Phase 1, SHADOW).
--
-- stop_forecast_mode on autogrid_settings is the operator switch (also
-- exposed in the frontend settings form):
--   OFF     — radar disabled, nothing computed or persisted;
--   SHADOW  — per-bot risk score computed every 5m, snapshots persisted,
--             band transitions logged + Telegram advisory, NO actions;
--   ACTIVE  — reserved for Phase 2 (actions); behaves as SHADOW until the
--             calibration gates pass.
--
-- bot_risk_snapshots is the calibration ledger: every scored tick joins the
-- bot's eventual outcome (closed ledger) so thresholds are tuned on
-- "would-have-saved vs actually-lost", never on intuition.

ALTER TABLE autogrid_settings ADD COLUMN IF NOT EXISTS stop_forecast_mode VARCHAR(12) NOT NULL DEFAULT 'OFF';

CREATE TABLE IF NOT EXISTS bot_risk_snapshots (
    id BIGSERIAL PRIMARY KEY,
    bot_id UUID NOT NULL,
    bot_number INT NOT NULL DEFAULT 0,
    symbol VARCHAR(32) NOT NULL,
    mode VARCHAR(12) NOT NULL,
    s1 NUMERIC(8,6) NOT NULL DEFAULT 0,
    s2 NUMERIC(8,6) NOT NULL DEFAULT 0,
    s3 NUMERIC(8,6) NOT NULL DEFAULT 0,
    s4 NUMERIC(8,6) NOT NULL DEFAULT 0,
    m5 NUMERIC(8,4) NOT NULL DEFAULT 1,
    score NUMERIC(8,6) NOT NULL DEFAULT 0,
    band SMALLINT NOT NULL DEFAULT 0,
    dist_to_stop_atr NUMERIC(10,4),
    inventory_skew NUMERIC(8,4),
    fleet_rho_neg NUMERIC(8,4),
    dom_slope_bps_h NUMERIC(10,4),
    total_pnl NUMERIC(20,8),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_bot_risk_bot_time ON bot_risk_snapshots(bot_id, captured_at DESC);
CREATE INDEX IF NOT EXISTS idx_bot_risk_time ON bot_risk_snapshots(captured_at DESC);
