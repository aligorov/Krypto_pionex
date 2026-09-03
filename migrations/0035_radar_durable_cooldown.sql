-- Migration 0035 (v2.0.68): durable dist-aware radar re-center cooldown.
--
-- radarMaybeRecenter now reads the bot's last radar action from
-- bot_execution_events instead of an in-memory map (a worker restart used to
-- hand every bot a fresh cooldown window it had just spent), and the window
-- itself is dist-aware (15m..2h, scaled by dist_to_stop² — see
-- radar_actions.go). The decision runs once per bot per manage pass, so the
-- lookup gets a dedicated partial index: only ADJUST_RANGE rows whose reason
-- marks them as radar re-centers (manage-loop shifts log the same event_type
-- with RANGE_BREAK_* reasons and must never arm this window).
--
-- The planned grid_bots.funding_paid_usdt column for REAL funding
-- reconciliation is NOT here: the Pionex client has no funding-fee history
-- method yet (official endpoint GET /uapi/v1/trade/fundingFee is
-- unimplemented in backend/internal/pionex/), so no code path writes the
-- column. It belongs in the migration that ships that client method.

CREATE INDEX IF NOT EXISTS idx_bot_execution_events_radar_actions
    ON bot_execution_events (bot_id, created_at DESC)
    WHERE event_type = 'ADJUST_RANGE'
      AND details->>'reason' IN ('RADAR_B3_RECENTER', 'RADAR_B4_ESCAPE');
