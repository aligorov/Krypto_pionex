-- v2.0.13: signal-gated tranche deployment + persisted peak PnL.
--
-- Tranches: with tranche_deploy_enabled, a new bot commits HALF the operator
-- budget; the manage loop tops up to the full tranche_base only after a
-- confirmed adverse excursion (price beyond ~0.75x ATR(1h) from entry with a
-- 15m turn confirmed) or a 24h time-box. Knife inventory drag is quadratic in
-- depth (U ~ N*D^2/2ms), so halving the initial N halves the damage of every
-- un-timed entry.
--
-- Peak PnL: TRAILING_TAKE_PROFIT and BREAKEVEN_LOCK were dead on both paths
-- because PeakPNL was recomputed as the current total every cycle. Persisted
-- peak_pnl_usdt makes the trailing policy stateful.
ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS tranche_deploy_enabled BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS peak_pnl_usdt NUMERIC(20,8) NOT NULL DEFAULT 0;

ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS peak_pnl_usdt NUMERIC(20,8) NOT NULL DEFAULT 0;

-- grid_bots needs the same per-bot tranche markers paper_grid_bots carries in
-- its model_state: deriving the pending tranche from live settings would turn
-- any budget raise into a fleet-wide real-money top-up (2026-08-20 audit).
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS model_state JSONB NOT NULL DEFAULT '{}'::jsonb;
