-- Migration 0038: wallet-equity truth (v2.0.75).
--
-- The bots' own PnL fields (profitReduce/profitExited mapped into
-- grid_bots.realized_pnl_usdt) do NOT contain entry/exit/invest_in fees —
-- Pionex charges those straight to the futures wallet, outside every PnL
-- attribution the API exposes. Summing bot PnL therefore overstates the
-- epoch by the accumulated fee bleed (~$3-8 on 25 entries / 9 closes at the
-- time of writing). This table snapshots the account-level USDT equity from
-- GET /uapi/v1/account/detail (assets + unrealizedPnL), so the operator's
-- headline number — "итоговый PnL" — can come from the wallet itself:
--
--   epoch PnL (wallet truth) = equity_now − equity_first_snapshot
--
-- The first row per account IS the epoch anchor: execution_mode history is
-- not retained anywhere else, and the snapshots only start being taken once
-- the worker runs, which is exactly the epoch the operator can verify.

CREATE TABLE IF NOT EXISTS account_equity_snapshots (
    id BIGSERIAL PRIMARY KEY,
    account_id UUID NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- equity = assets + unrealizedPnL (USDT cross-margin wallet, net of debts)
    equity_usdt NUMERIC(20,8) NOT NULL,
    assets_usdt NUMERIC(20,8) NOT NULL DEFAULT 0,
    available_usdt NUMERIC(20,8) NOT NULL DEFAULT 0,
    unrealized_pnl_usdt NUMERIC(20,8) NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_account_equity_account_time
    ON account_equity_snapshots (account_id, captured_at DESC);

-- Bounded retention: the 5-minute cadence produces ~288 rows/day; a year of
-- history is ample for epoch accounting.
CREATE INDEX IF NOT EXISTS idx_account_equity_captured_at
    ON account_equity_snapshots (captured_at DESC);
