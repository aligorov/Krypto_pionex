-- Migration 0055: Adaptive Target Prices, Structural Stop Losses, and Native Card SL/TP Telemetry.
-- Replaces rigid $9/$18 artificial profit boundaries with individualized L2 order book wall / S/R targets,
-- dynamic trailing stops, and native Pionex bot card parameters.

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS smart_exit_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS ofi_harvest_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS ou_rotation_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS min_risk_reward NUMERIC(10, 4) NOT NULL DEFAULT 1.8;

ALTER TABLE autogrid_candidates
    ADD COLUMN IF NOT EXISTS target_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS stop_loss_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS stop_loss_high NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS adaptive_strategy VARCHAR(32),
    ADD COLUMN IF NOT EXISTS risk_reward_ratio NUMERIC(10, 4);

ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS target_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS stop_loss_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS stop_loss_high NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS trailing_sl_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS adaptive_strategy VARCHAR(32),
    ADD COLUMN IF NOT EXISTS risk_reward_ratio NUMERIC(10, 4);

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS target_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS stop_loss_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS stop_loss_high NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS trailing_sl_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS adaptive_strategy VARCHAR(32),
    ADD COLUMN IF NOT EXISTS risk_reward_ratio NUMERIC(10, 4);
