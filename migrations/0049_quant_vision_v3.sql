-- Migration 0049 (v2.0.114): Quant & Vision Engine v3.0 parameters.
-- Safe, idempotent addition of vision, wick shield, fleet delta, and Gaussian density controls.

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS wick_shield_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS wick_grace_sec INT NOT NULL DEFAULT 90,
    ADD COLUMN IF NOT EXISTS fleet_max_net_delta_usdt NUMERIC(10,2) NOT NULL DEFAULT 1200.00,
    ADD COLUMN IF NOT EXISTS universe_scan_cap INT NOT NULL DEFAULT 250,
    ADD COLUMN IF NOT EXISTS max_spread_pct NUMERIC(6,4) NOT NULL DEFAULT 0.0020,
    ADD COLUMN IF NOT EXISTS gaussian_density_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS orderbook_profiler_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS knife_pause_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS min_depth_cushion_ratio NUMERIC(10,2) NOT NULL DEFAULT 50.00;

-- Telemetry columns on grid_bots and paper_grid_bots for vision audit
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS vision_context JSONB DEFAULT '{}'::jsonb;

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS vision_context JSONB DEFAULT '{}'::jsonb;
