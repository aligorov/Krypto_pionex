-- Migration 0048 (v2.0.113): supervision floor PnL for conservative risk tracking across exchange rebases.
--
-- When an exchange re-bases positionOpenPrice across a range shift or invest_in (tranche-2),
-- the API payload discards the historical cost basis. The raw unrealized PnL is kept on
-- unrealized_pnl_usdt for raw display parity, while supervision_floor_pnl_usdt tracks the
-- conservative floor: min(raw, raw + rebasePool) so risk supervision (stops, radar) never
-- suffers from false optimism on underwater positions.

ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS supervision_floor_pnl_usdt NUMERIC(20, 8) DEFAULT 0;

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS supervision_floor_pnl_usdt NUMERIC(20, 8) DEFAULT 0;
