-- 0064 v2.0.199: Diamond risk safeguards
-- 1. directional_enabled: default false, disabling unprofitable directional LONG/SHORT grids until proven in forward testing
-- 2. max_leverage_cap: default 2, capping maximum leverage to safe 2x
-- 3. max_loss_target_multiplier: default 2.80, capping neutral grid stop-loss to 2.8x of its take-profit target to fix the 1:10 ruin asymmetry

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS directional_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS max_leverage_cap INT NOT NULL DEFAULT 2,
    ADD COLUMN IF NOT EXISTS max_loss_target_multiplier NUMERIC(6,2) NOT NULL DEFAULT 2.80;
