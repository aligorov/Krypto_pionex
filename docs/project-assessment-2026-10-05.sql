-- Audit diagnostics. SELECT/WITH only; no keys, account credentials or writes.
-- Run in a read-only PostgreSQL session against a consistent snapshot when
-- possible. Current counts can differ from the 2026-10-05 report.
-- Through autogrid_sql, execute each statement separately (table whitelist).

-- 1. Attempts versus remotely created terminal bots; missing finals are NOT 0.
SELECT execution_mode, status, COUNT(*) AS attempts,
       COUNT(*) FILTER (WHERE bu_order_id IS NOT NULL) AS remote_ids,
       COUNT(*) FILTER (WHERE realized_pnl_usdt IS NULL) AS missing_finals,
       SUM(realized_pnl_usdt) AS stored_final_usdt
FROM grid_bots
WHERE execution_mode = 'REAL'
GROUP BY execution_mode, status
ORDER BY status;

-- 2. Stored-final and the project's additional-entry-fee net convention.
-- This duplicates equity.go's convention; it is NOT an independent exchange
-- wallet reconciliation. Never subtract all fees again from a net-close final.
WITH known AS (
    SELECT id, closed_at, direction, quote_investment,
           realized_pnl_usdt AS stored_final,
           realized_pnl_usdt - (
               COALESCE(fees_paid_usdt, 0) -
               COALESCE(NULLIF(model_state->>'closeCostUsdt', '')::numeric, 0)
           ) AS project_net
    FROM grid_bots
    WHERE execution_mode = 'REAL'
      AND bu_order_id IS NOT NULL
      AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
      AND realized_pnl_usdt IS NOT NULL
), metric_rows AS (
    SELECT 'stored_final' AS convention, stored_final AS pnl FROM known
    UNION ALL
    SELECT 'project_net', project_net FROM known
)
SELECT convention, COUNT(*) AS n, SUM(pnl) AS pnl_usdt,
       AVG(pnl) AS mean_per_bot_usdt,
       100.0 * COUNT(*) FILTER (WHERE pnl > 0) / COUNT(*) AS win_rate_pct,
       SUM(pnl) FILTER (WHERE pnl > 0) /
         NULLIF(-SUM(pnl) FILTER (WHERE pnl < 0), 0) AS profit_factor,
       AVG(pnl) FILTER (WHERE pnl > 0) AS mean_win,
       AVG(pnl) FILTER (WHERE pnl < 0) AS mean_loss
FROM metric_rows
GROUP BY convention;

-- 3. Direction partitions with the same accounting convention.
WITH known AS (
    SELECT direction, realized_pnl_usdt AS final,
           realized_pnl_usdt - (
               COALESCE(fees_paid_usdt, 0) -
               COALESCE(NULLIF(model_state->>'closeCostUsdt', '')::numeric, 0)
           ) AS net
    FROM grid_bots
    WHERE execution_mode = 'REAL' AND bu_order_id IS NOT NULL
      AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
      AND realized_pnl_usdt IS NOT NULL
)
SELECT direction, COUNT(*) AS n, SUM(final) AS stored_final,
       SUM(net) AS project_net, AVG(net) AS net_mean,
       100.0 * COUNT(*) FILTER (WHERE net > 0) / COUNT(*) AS net_win_rate_pct,
       SUM(net) FILTER (WHERE net > 0) /
         NULLIF(-SUM(net) FILTER (WHERE net < 0), 0) AS net_profit_factor
FROM known
GROUP BY direction ORDER BY direction;

-- 4. Final source and unknown result coverage.
SELECT COALESCE(model_state->>'finalProfitSource', 'missing') AS final_source,
       COUNT(*) AS n,
       COUNT(*) FILTER (WHERE realized_pnl_usdt IS NULL) AS unknown,
       SUM(realized_pnl_usdt) AS stored_final
FROM grid_bots
WHERE execution_mode = 'REAL' AND bu_order_id IS NOT NULL
  AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
GROUP BY COALESCE(model_state->>'finalProfitSource', 'missing');

-- 5. UTC day of concentrated loss. Changing the timezone changes day cohorts.
SELECT bot_number, symbol, direction, closed_reason, quote_investment, leverage,
       realized_pnl_usdt, pnl_target_usdt, max_loss_usdt, peak_pnl_usdt,
       created_at, closed_at, model_state->>'finalProfitSource' AS source
FROM grid_bots
WHERE execution_mode = 'REAL' AND bu_order_id IS NOT NULL
  AND closed_at >= TIMESTAMPTZ '2026-10-02 00:00:00+00'
  AND closed_at < TIMESTAMPTZ '2026-10-03 00:00:00+00'
  AND realized_pnl_usdt IS NOT NULL
ORDER BY realized_pnl_usdt;

-- 6. Target/loss ratio is not the stored adaptive price-distance R:R.
SELECT COUNT(*) AS n,
       COUNT(*) FILTER (WHERE pnl_target_usdt / max_loss_usdt < 0.2)
         AS target_loss_below_point2,
       MIN(pnl_target_usdt / max_loss_usdt) AS minimum_target_loss,
       AVG(pnl_target_usdt / max_loss_usdt) AS average_target_loss,
       AVG(risk_reward_ratio) AS average_stored_rr
FROM grid_bots
WHERE execution_mode = 'REAL' AND bu_order_id IS NOT NULL
  AND direction = 'NEUTRAL' AND max_loss_usdt > 0
  AND created_at >= TIMESTAMPTZ '2026-09-28 00:00:00+00';

-- 7. Legacy simulated flag versus the decision-intelligence state contract.
SELECT simulated, pos_state, calc_state, COUNT(*) AS n,
       COUNT(*) FILTER (WHERE outcome_pnl_usdt IS NOT NULL) AS with_outcome,
       COUNT(*) FILTER (WHERE decision_id IS NOT NULL) AS linked_decisions
FROM shadow_candidates
GROUP BY simulated, pos_state, calc_state;

-- 8. Journal completeness. Do not treat single-gate INTERMEDIATE ALLOW as
-- a full FINAL admission. The bound matches the observed new-journal epoch.
SELECT fleet, COUNT(*) AS n,
       COUNT(*) FILTER (WHERE config_snapshot = '{}'::jsonb) AS empty_config,
       COUNT(*) FILTER (WHERE stage = 'FINAL') AS final_decisions,
       COUNT(*) FILTER (WHERE jsonb_array_length(gate_trace) > 1) AS multi_gate,
       COUNT(*) FILTER (WHERE outcome = 'ALLOW') AS allows,
       COUNT(*) FILTER (WHERE direction_after IS NULL) AS missing_direction,
       COUNT(*) FILTER (WHERE price_at_decision IS NULL) AS missing_price
FROM entry_decisions
WHERE created_at >= TIMESTAMPTZ '2026-10-01 19:11:40+00'
GROUP BY fleet;

-- 9. Contradictory backtest metrics. Pair EV != bot terminal net return.
SELECT COUNT(*) AS n,
       COUNT(*) FILTER (
           WHERE (result->>'net_ev')::numeric > 0
             AND (result->>'oos_return_pct')::numeric < 0
       ) AS positive_ev_negative_oos,
       COUNT(*) FILTER (
           WHERE (result->>'win_rate')::numeric = 100
             AND (result->>'oos_return_pct')::numeric < 0
       ) AS wr100_negative_oos,
       COUNT(*) FILTER (
           WHERE COALESCE((result->>'sample_sufficient')::boolean, false)
             AND COALESCE((result->>'ci95_positive')::boolean, false)
             AND (result->>'oos_return_pct')::numeric BETWEEN -1 AND 0
             AND (result->>'oos_return_pct')::numeric > -1
             AND (result->>'oos_return_pct')::numeric < 0
       ) AS sufficient_positive_ci_weak_negative
FROM backtest_jobs
WHERE status = 'DONE' AND result->>'engine_version' = 'v173';

-- 10. Corroboration of F01 from live logs.
SELECT occurred_at, level, message, fields->>'error' AS error
FROM application_logs
WHERE message = 'shadow capture: decision linkage failed'
ORDER BY occurred_at DESC LIMIT 5;

-- 11. Historical failed submissions, not necessarily current regressions.
SELECT LEFT(COALESCE(last_error, 'missing'), 180) AS error,
       COUNT(*) AS n, MAX(created_at) AS latest
FROM grid_bots
WHERE execution_mode = 'REAL' AND status = 'FAILED'
GROUP BY LEFT(COALESCE(last_error, 'missing'), 180)
ORDER BY n DESC LIMIT 12;
