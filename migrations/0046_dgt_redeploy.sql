-- Migration 0046 (v2.0.89 part B): DGT break re-deploy switch.
--
-- dgt_redeploy_enabled arms the Deep-Grid-Trading break re-start (arXiv
-- 2506.11921: "from zero expectation to outperformance"). When the manage
-- loop closes a bot on a RANGE_BREAK_* decision, the fleet immediately
-- re-opens the SAME symbol with the SAME slot capital, centered on the BREAK
-- price with fresh-geometry bounds — for BOTH paper and REAL, symmetrically.
--
-- Why this is a re-center and NOT a cooldown re-entry: the per-symbol
-- protective-close cooldown (v2.0.28) exists to stop a pair from re-entering
-- THE SAME place that just killed a bot. A DGT re-deploy enters a NEW
-- geometry (center = break price, fresh ATR/HAR width, fresh adjustment
-- budget) the old grid never traded, so the deploy paths' cooldown gate is
-- deliberately not consulted here; the economic/macro/portfolio gates and
-- the kill switch still run, and a per-symbol 24h re-deploy ladder cap
-- bounds the worst runaway-tape case.
--
-- The switch is one column for BOTH execution modes (paper/REAL parity is
-- the acceptance contract); DEFAULT TRUE per the research directive.

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS dgt_redeploy_enabled BOOLEAN NOT NULL DEFAULT TRUE;

-- The runaway-ladder cap counts DGT_REDEPLOY events per symbol per 24h —
-- same durable partial-index shape the radar cooldowns use (the 0035/0042
-- pattern).
CREATE INDEX IF NOT EXISTS idx_bot_execution_events_dgt_redeploy
    ON bot_execution_events (symbol, created_at DESC)
    WHERE event_type = 'DGT_REDEPLOY';
