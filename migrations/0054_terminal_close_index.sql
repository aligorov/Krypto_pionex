-- Migration 0054 (v2.0.142, audit P2c index half): terminal-close recency
-- indexes for the joint circuit breaker and the DGT chain-loss budget.
--
-- jointProtectiveClosesLastHour (entry_chain.go) runs once per admission
-- evaluation and scans terminal grid_bots by a recency window over
-- COALESCE(closed_at, updated_at) — REAL leg WHERE status IN
-- ('STOPPED','LIQUIDATED'), paper leg WHERE status = 'COMPLETED' on
-- closed_at. dgtSharedGateBlockers' chain-loss budget (grid_lifecycle_
-- policy.go) filters the same REAL shape over 24h (status IN
-- ('STOPPED','COMPLETED','LIQUIDATED')). Without an index each probe is a
-- full scan of every REAL bot row ever (audit P2c). Partial expression
-- index — only terminal rows are ever interesting; COALESCE over two
-- timestamptz columns is immutable, so the expression is indexable on
-- Postgres 16, and the status list is the union of both queries' predicates
-- so the planner can serve either from the one index.

CREATE INDEX IF NOT EXISTS idx_grid_bots_terminal_recency
    ON grid_bots (COALESCE(closed_at, updated_at) DESC)
    WHERE status IN ('STOPPED', 'LIQUIDATED', 'COMPLETED');

-- Paper twin: the breaker's paper leg probes paper_grid_bots
-- (status = 'COMPLETED', closed_at > NOW() - INTERVAL '1 hour'). The 0047
-- closed-ledger index leads on settings_id; this partial recency index is
-- the shape the breaker's window predicate wants, on exactly the COMPLETED
-- cohort it reads.

CREATE INDEX IF NOT EXISTS idx_paper_grid_bots_completed_recency
    ON paper_grid_bots (closed_at DESC)
    WHERE status = 'COMPLETED';
