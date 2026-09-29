-- Migration 0057: Reopen terminal rows where positive cash settlements were mistakenly
-- refused by the sanity gate and clamped to telemetry estimate / -max_loss (e.g. CRWVX #1440).
-- When re-opened to TERMINAL_FINAL_PENDING_EXCHANGE, the re-check sweep resolves the true
-- unlock_identity (+0.00811818 USDT) from Pionex finished grid orders.

UPDATE grid_bots
SET reconciliation_state = 'TERMINAL_FINAL_PENDING_EXCHANGE',
    realized_pnl_usdt = COALESCE(peak_pnl_usdt, realized_pnl_usdt),
    updated_at = NOW()
WHERE status = 'STOPPED'
  AND bu_order_id IS NOT NULL
  AND model_state->>'finalProfitSource' = 'telemetry_net_close'
  AND closed_at >= NOW() - INTERVAL '7 days';
