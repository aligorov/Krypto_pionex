-- Migration 0047 (v2.0.94): closed-ledger index for the cumulative symbol
-- cooldown.
--
-- loadLosingSymbolCooldowns (worker.go) scans the trailing 7 days of
-- COMPLETED paper closes per deploy round — twice (paper loop + REAL loop).
-- The only existing paper_grid_bots index is the partial RUNNING uniqueness
-- index (migration 0004); the closed ledger is append-only and seq-scanned
-- forever without this. Partial index keeps it small: only COMPLETED rows,
-- which is exactly the cohort every closed-ledger query reads (cooldown,
-- weekly mining, win-rate analytics).

CREATE INDEX IF NOT EXISTS idx_paper_grid_bots_closed_ledger
    ON paper_grid_bots (settings_id, closed_at)
    WHERE status = 'COMPLETED';
