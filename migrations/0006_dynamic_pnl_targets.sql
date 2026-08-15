-- Migration 0006: Dynamic per-bot PnL targets driven by market indicators

-- Mode: DYNAMIC (targets computed per bot from AI Kit volatility / scanner
-- ATR+sigma and model drawdown) or FIXED (operator-entered USDT amounts).
ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS pnl_target_mode VARCHAR(16) NOT NULL DEFAULT 'DYNAMIC';

-- Per-bot targets captured at deploy time so supervision uses exactly what
-- the market offered when the bot opened (falls back to settings when NULL).
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS pnl_target_usdt NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS max_loss_usdt NUMERIC(20, 8);

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS pnl_target_usdt NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS max_loss_usdt NUMERIC(20, 8);
