-- Migration 0045 (v2.0.89): the ledger stops lying — round-trip fees are
-- booked, and every terminal final that is not the exchange's own netted
-- total re-settles at the telemetry-net-close estimate.
--
-- Context (REAL epoch 2026-09-03..05): the operator's futures wallet closed
-- at −$31 while the ledger summed +3.08. Two structural lies:
--   1. grid_funding_residual / grid_funding_flat "finals" — profitReduce +
--      funding without the position-close leg (+$20.48 of fictional profit
--      across the fleet: FARTCOIN +2.35 on an ANTI_HUNT loss, CRV +6.01 …);
--   2. no fee leg at all — the wallet paid taker on every deploy/pour and
--      taker+slippage on every close while no ledger row carried it, and the
--      stop executions landed below the last telemetry mark (APT's native
--      stop executed at −12 with the last mark at −2.27).
--
-- Four durable pieces (mirrors of the Go paths in autogrid/finalprofit.go
-- and the worker settle paths; the integration test pins SQL == Go):
--   1. grid_bots.fees_paid_usdt + paper_grid_bots.fees_paid_usdt — the
--      cumulative fee ledger (entry fees at deploy/pour + close costs);
--   2. entry-fee backfill: taker 0.05% × investment × leverage for every REAL
--      bot (tranche-2 rows already carry the doubled base in
--      quote_investment, so deploy fee + pour fee fall out of one
--      expression — the same pionex.EntryFeeUSDT model the code books);
--   3. reopen every terminal final that is not an exchange total
--      (finalProfitSource not in profit_exited/total_profit_alias) — the
--      v2.0.75–88 markers (grid_funding_*, refused_*, none) all describe
--      chain legs that are no longer accepted as finals;
--   4. re-settle those terminals IN SQL with the v2.0.89 formula
--      (final = last telemetry total − 0.1% × inventory, floored at
--      −max_loss − close cost for executed stops; no telemetry → NULL final)
--      so the epoch flips to the honest figure the moment the migration
--      lands, without waiting for the worker's 30-day backfill sweep.

-- (1) schema -----------------------------------------------------------------
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS fees_paid_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0;
ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS fees_paid_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0;

-- (2) entry-fee backfill (REAL bots; paper books fees live from v2.0.89) -----
UPDATE grid_bots g
SET fees_paid_usdt = fees_paid_usdt + f.fee,
    model_state = jsonb_set(COALESCE(g.model_state, '{}'::jsonb), '{entryFeeUsdt}',
        to_jsonb(f.fee::TEXT)),
    updated_at = NOW()
FROM (
    SELECT id, ROUND(0.0005 * quote_investment * leverage, 8) AS fee
    FROM grid_bots
    WHERE execution_mode = 'REAL'
      AND bu_order_id IS NOT NULL
      AND COALESCE(model_state->>'entryFeeUsdt', '') = ''
) f
WHERE g.id = f.id;

-- (3)+(4) reopen and re-settle the non-exchange-total terminals --------------
WITH reopened AS (
    SELECT g.id,
           COALESCE(g.max_loss_usdt, 0) AS max_loss,
           COALESCE(g.closed_reason, '') AS reason,
           COALESCE(g.closed_at, g.updated_at) AS anchor
    FROM grid_bots g
    WHERE g.bu_order_id IS NOT NULL
      AND g.status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
      AND COALESCE(g.model_state->>'finalProfitSource', '')
            NOT IN ('profit_exited', 'total_profit_alias', 'telemetry_net_close')
),
last_tel AS (
    SELECT DISTINCT ON (r.id)
           r.id, r.max_loss, r.reason,
           t.total_pnl,
           COALESCE(t.inventory_notional, 0) AS inv
    FROM reopened r
    LEFT JOIN bot_telemetry t
           ON t.bot_id = r.id AND t.captured_at <= r.anchor
    ORDER BY r.id, t.captured_at DESC
),
estimates AS (
    SELECT lt.id,
           lt.inv,
           ROUND(lt.inv * 0.001, 8) AS close_cost,
           ROUND(
             LEAST(
               lt.total_pnl - lt.inv * 0.001,
               CASE WHEN upper(lt.reason) IN
                         ('STOP_LOSS', 'STOP_LOSS_NATIVE', 'LIQUIDATION',
                          'FORCE_LIQUIDATION', 'LOSS_STOP')
                    AND lt.max_loss > 0
                    THEN -lt.max_loss - lt.inv * 0.001
                    ELSE lt.total_pnl - lt.inv * 0.001 END
             ), 8) AS final_net
    FROM last_tel lt
    WHERE lt.total_pnl IS NOT NULL
)
UPDATE grid_bots g
SET realized_pnl_usdt = e.final_net,
    unrealized_pnl_usdt = 0,
    fees_paid_usdt = fees_paid_usdt + e.close_cost,
    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
    model_state = jsonb_set(
        jsonb_set(COALESCE(g.model_state, '{}'::jsonb), '{closeCostUsdt}',
            to_jsonb(e.close_cost::TEXT)),
        '{finalProfitSource}', to_jsonb('telemetry_net_close'::TEXT)),
    updated_at = NOW()
FROM estimates e
WHERE g.id = e.id;

-- Terminals with no telemetry at all: NULL final, marked unknown — the figure
-- is never invented (the epoch counts them as unknown, out of the sum).
UPDATE grid_bots g
SET realized_pnl_usdt = NULL,
    unrealized_pnl_usdt = 0,
    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
    model_state = jsonb_set(COALESCE(g.model_state, '{}'::jsonb),
        '{finalProfitSource}', to_jsonb('none'::TEXT)),
    updated_at = NOW()
FROM (
    SELECT DISTINCT ON (r.id) r.id, t.total_pnl
    FROM (
        SELECT g2.id, COALESCE(g2.closed_at, g2.updated_at) AS anchor
        FROM grid_bots g2
        WHERE g2.bu_order_id IS NOT NULL
          AND g2.status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
          AND COALESCE(g2.model_state->>'finalProfitSource', '')
                NOT IN ('profit_exited', 'total_profit_alias', 'telemetry_net_close')
    ) r
    LEFT JOIN bot_telemetry t
           ON t.bot_id = r.id AND t.captured_at <= r.anchor
    ORDER BY r.id, t.captured_at DESC
) probe
WHERE g.id = probe.id AND probe.total_pnl IS NULL;
