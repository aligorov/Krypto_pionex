-- Migration 0042 (v2.0.84): radar auto-close mode.
--
-- radar_autoclose_mode on autogrid_settings is the operator switch that lets
-- the stop-radar CLOSE a running bot instead of only re-centering / warning:
--
--   OFF    — default, nothing is ever auto-closed (ships OFF in prod until
--            the operator explicitly opts in);
--   BAND3  — close when band>=3 AND the bot is under water (floating PnL<0)
--            AND the signal dwelt >=3 snapshots AND the bot is older than
--            30 minutes AND the per-bot 1h cooldown allows it;
--   STRICT — the BAND3 gates plus dist_to_stop < 0.5 ATR-sigma (fires only
--            when the barrier is genuinely near).
--
-- Provenance — the 2026-09-03T20:35Z..09-04T19:29Z REAL backtest over
-- bot_risk_snapshots (13 REAL bots, 24 band>=3 episodes):
--   recall 8/8 loss-closures had band>=3 within 3h before the stop;
--   precision 9/24 episodes ended in a stop (64% among episodes that started
--   under water); EV of the BAND3 policy ≈ +$7..+13 over 23h, but the whole
--   surplus is the single SNXXX #669 close (+$10.2 of +$16.4 saved) —
--   CRV #675 (+$4.9 alive) and FARTCOIN #679 (+$1.1 closed green) are the
--   standing counter-examples, hence OFF by default and the never-close-green
--   invariant in radar_actions.go.

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS radar_autoclose_mode VARCHAR(8) NOT NULL DEFAULT 'OFF';

-- The auto-close cooldown reads the newest RADAR_AUTOCLOSE event per bot
-- (durable, restart-proof — the 0035 pattern); give it the same partial
-- index shape the re-center cooldown uses.
CREATE INDEX IF NOT EXISTS idx_bot_execution_events_radar_autoclose
    ON bot_execution_events (bot_id, created_at DESC)
    WHERE event_type = 'RADAR_AUTOCLOSE';
