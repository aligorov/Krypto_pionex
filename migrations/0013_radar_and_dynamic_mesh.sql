-- Migration 0013: Radar, Anti-Hunt Shield, Break-Even Floor, and Dynamic Mesh
ALTER TABLE autogrid_settings
ADD COLUMN IF NOT EXISTS min_grid_step_pct NUMERIC NOT NULL DEFAULT 0.30,
ADD COLUMN IF NOT EXISTS anti_hunt_atr_mult NUMERIC NOT NULL DEFAULT 1.5,
ADD COLUMN IF NOT EXISTS break_even_profit_pct NUMERIC NOT NULL DEFAULT 1.5,
ADD COLUMN IF NOT EXISTS session_adaptation_enabled BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE paper_grid_bots
ADD COLUMN IF NOT EXISTS confluence_score NUMERIC DEFAULT 0,
ADD COLUMN IF NOT EXISTS grid_step_pct NUMERIC DEFAULT 0,
ADD COLUMN IF NOT EXISTS break_even_price NUMERIC DEFAULT 0,
ADD COLUMN IF NOT EXISTS anti_hunt_stop_price NUMERIC DEFAULT 0;

ALTER TABLE grid_bots
ADD COLUMN IF NOT EXISTS confluence_score NUMERIC DEFAULT 0,
ADD COLUMN IF NOT EXISTS grid_step_pct NUMERIC DEFAULT 0,
ADD COLUMN IF NOT EXISTS break_even_price NUMERIC DEFAULT 0,
ADD COLUMN IF NOT EXISTS anti_hunt_stop_price NUMERIC DEFAULT 0;

CREATE TABLE IF NOT EXISTS autogrid_radar_watchlist (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    settings_id UUID NOT NULL REFERENCES autogrid_settings(id) ON DELETE CASCADE,
    symbol TEXT NOT NULL,
    direction TEXT NOT NULL DEFAULT 'NEUTRAL',
    target_entry_price NUMERIC NOT NULL,
    current_price NUMERIC NOT NULL,
    distance_pct NUMERIC NOT NULL DEFAULT 0,
    confluence_score NUMERIC NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'WATCHING', -- 'WATCHING', 'ARMED', 'TRIGGERED', 'EXPIRED'
    model_assumptions JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(settings_id, symbol)
);

CREATE INDEX IF NOT EXISTS idx_radar_watchlist_status ON autogrid_radar_watchlist(settings_id, status);
