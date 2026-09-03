-- Migration 0037 (v2.0.72): stop-radar REAL port.
--
-- The radar used to score paper_grid_bots only, so the REAL fleet flew
-- without radar protection and without Telegram advisories. radarPass now
-- receives REAL inputs too (grid_bots RUNNING with a remote buOrderId),
-- which makes two rows distinguishable everywhere the ledger is read:
--  * bot_risk_snapshots gains bot_source ('PAPER'/'REAL'), defaulting to
--    'PAPER' so every pre-existing calibration row keeps its meaning
--    without a backfill;
--  * bot_execution_events already carries bot_source (0015) and
--    bot_id is a TEXT id on both fleets, so the durable radar cooldown
--    (0035) gates REAL ids unchanged — no structural change there.
-- Budget sharing with the manage path is likewise structural: both paths
-- increment the same grid_bots.adjustments_count.

ALTER TABLE bot_risk_snapshots
    ADD COLUMN IF NOT EXISTS bot_source VARCHAR(16) NOT NULL DEFAULT 'PAPER';

-- REAL snapshots must be reachable per bot+source like the paper ones.
CREATE INDEX IF NOT EXISTS idx_bot_risk_source_time
    ON bot_risk_snapshots (bot_source, captured_at DESC);
