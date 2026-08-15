-- Migration 0008: AI Kit auto-tuning while the autopilot runs

-- When enabled and RUNNING, the worker periodically re-samples the native
-- AI Kit distribution and nudges a whitelisted subset of scanner settings
-- (volatility band, drawdown cap, leverage) with per-step clamps; everything
-- else (mode, account, budget, bots) stays operator-only.
ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS ai_autotune_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS ai_autotune_interval_seconds INT NOT NULL DEFAULT 3600,
    ADD COLUMN IF NOT EXISTS last_autotune_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_autotune_notes TEXT;
