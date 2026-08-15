-- Migration 0005: Per-bot PnL management, AI Kit advisory and lifecycle accounting

-- 1. AutoGrid PnL management settings (Zero-ENV: everything lives in PostgreSQL)
ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS pnl_target_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_loss_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS manage_interval_seconds INT NOT NULL DEFAULT 60,
    ADD COLUMN IF NOT EXISTS range_break_buffer_pct NUMERIC(8, 4) NOT NULL DEFAULT 1.0,
    ADD COLUMN IF NOT EXISTS max_adjustments_per_bot INT NOT NULL DEFAULT 3,
    ADD COLUMN IF NOT EXISTS ai_kit_enabled BOOLEAN NOT NULL DEFAULT true;

-- 2. Grid bot PnL & management accounting
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS realized_pnl_usdt NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS unrealized_pnl_usdt NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS closed_reason VARCHAR(64),
    ADD COLUMN IF NOT EXISTS adjustments_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;

-- 3. Paper bots mirror the same accounting so PAPER mode exercises the full cycle
ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS closed_reason VARCHAR(64);

-- 4. Hot path indexes (risk engine + worker polls filter by account/status)
CREATE INDEX IF NOT EXISTS idx_grid_bots_account_status ON grid_bots (account_id, status);
CREATE INDEX IF NOT EXISTS idx_grid_bots_autogrid_status ON grid_bots (autogrid_settings_id, status);
CREATE INDEX IF NOT EXISTS idx_autogrid_scan_runs_settings_completed ON autogrid_scan_runs (settings_id, completed_at DESC);

-- 5. Align the scanner friction model with the official Pionex fee schedule
--    (futures maker 0.02%, taker 0.05%; grid orders fill mostly as maker).
UPDATE autogrid_settings
    SET fee_bps = 5.0, slippage_bps = 2.0
    WHERE fee_bps > 5.0 OR slippage_bps > 2.0;
