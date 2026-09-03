-- Migration 0036 (v2.0.68 FIX-1): REAL funding reconciliation.
--
-- The paper simulator has booked funding into realized PnL since v2.0.6
-- (paper_grid_bots.funding_paid_usdt), while REAL grid_bots rows carried no
-- funding state at all — the manage loop hard-coded telemetry funding to 0.
-- The Pionex client now implements the official GET /uapi/v1/trade/fundingFee
-- (signed records, positive = paid), so the REAL path gets the same two
-- durable columns the paper path already has:
--  * funding_paid_usdt — cumulative signed net funding PAID (positive = paid,
--    negative = received), accumulated from exchange records only;
--  * last_funding_reconcile_at — the reconciliation window anchor; NULL means
--    the window starts at the bot's created_at. The manage loop refreshes it
--    at most once per 30 minutes per bot and NEVER advances it on a failed
--    fetch, so skipped records are retried instead of being lost.

ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS funding_paid_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_funding_reconcile_at TIMESTAMPTZ;

-- Repair (found by the funding integration test): the REAL remote-truth
-- persist in the manage loop has written trough_pnl_usdt since v2.0.45, but
-- only paper_grid_bots ever got the column (0028) — grid_bots never did, so
-- that UPDATE failed on every pass and its error was swallowed, silently
-- dropping realized/unrealized/peak persistence for RUNNING REAL bots. The
-- column restores the UPDATE exactly as written; existing rows backfill to 0,
-- matching the paper table's semantics.
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS trough_pnl_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0;
