-- Migration 0041: TOTAL PnL = bot aggregate (v2.0.83).
--
-- Root cause closed with raw /uapi/v1/account/detail captures (v2.0.82): on
-- this fleet every coin row answers zero and positions is [] — the grid
-- margins and the floating PnL live INSIDE the isolated futures-grid bots,
-- invisible to every account-level endpoint. The wallet-truth capture of
-- v2.0.75–82 could therefore never see money that exists; the application
-- truth (what the Pionex app itself shows) is the AGGREGATE OVER BOTS:
--
--   epoch_pnl = Σ running (grid profit + funding + floating)
--             + Σ closed-of-epoch (settled final; NULL → last telemetry
--               total before close, marked estimated)
--
-- Two durable pieces:
--  1. account_equity_snapshots.source — rows are tagged by their semantics:
--     'bot_aggregate' (v2.0.83+: assets_usdt = Σ running investment,
--     unrealized_pnl_usdt = Σ running floating, equity_usdt = wallet USDT +
--     Σ running (investment + realized + floating)) vs the legacy
--     'account_detail' wallet snapshots.
--  2. app_config pnl_epoch_started_at — the epoch anchor. Backfilled with
--     the first REAL grid bot created after 2026-09-03 16:30Z (execution
--     mode REAL went live 16:33Z that day); a plain constant when no such
--     row exists (fresh installs). Durable and operator-resettable: UPDATE
--     app_config to move the epoch boundary.

ALTER TABLE account_equity_snapshots
    ADD COLUMN IF NOT EXISTS source VARCHAR(32) NOT NULL DEFAULT 'account_detail';

INSERT INTO app_config (key, value, description)
VALUES (
    'pnl_epoch_started_at',
    COALESCE(
        (SELECT to_jsonb(to_char(MIN(g.created_at) AT TIME ZONE 'UTC',
                                   'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
         FROM grid_bots g
         WHERE g.execution_mode = 'REAL'
           AND g.bu_order_id IS NOT NULL
           AND g.created_at > TIMESTAMPTZ '2026-09-03 16:30:00Z'),
        to_jsonb('2026-09-03T16:33:00Z'::TEXT)
    ),
    'TOTAL PnL epoch anchor (v2.0.83 bot-aggregate truth): REAL grid bots created at-or-after this instant count toward the epoch PnL'
)
ON CONFLICT (key) DO NOTHING;
