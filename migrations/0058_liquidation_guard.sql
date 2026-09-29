-- v2.0.161 liquidation guard: deploy-time exchange estimates from
-- futuresGrid/checkParams (estimate_liquidation_price_up/down) and the
-- running liquidation price reported by the bot detail feed. The stop
-- ladder and the proximity guard both read these.
ALTER TABLE grid_bots ADD COLUMN IF NOT EXISTS liq_price_up NUMERIC;
ALTER TABLE grid_bots ADD COLUMN IF NOT EXISTS liq_price_down NUMERIC;
ALTER TABLE grid_bots ADD COLUMN IF NOT EXISTS liq_price NUMERIC;
