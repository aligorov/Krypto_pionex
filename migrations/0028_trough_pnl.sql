-- Migration 0028: trough PnL telemetry next to the existing peak.
-- The closed ledger carries peak_pnl_usdt only; worst adverse excursion per
-- bot (trough) has been inference from klines until now. Persisting it makes
-- stop-clamp decisions data-driven instead of estimated (agent audit
-- 2026-08-31: the $8 maxLoss clamp counterfactual needed entry-time troughs
-- that the DB did not have).

ALTER TABLE paper_grid_bots ADD COLUMN IF NOT EXISTS trough_pnl_usdt NUMERIC(20,8) NOT NULL DEFAULT 0;
