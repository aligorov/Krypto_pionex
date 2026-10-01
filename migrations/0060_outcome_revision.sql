-- v2.0.169: outcome revision tracking — the confirmed exchange final updates
-- the stored estimate (first-outcome-wins guard removed for REMOTE_CONFIRMED
-- finals; the previous value is preserved in outcome_prev_*).
ALTER TABLE autogrid_candidates ADD COLUMN IF NOT EXISTS outcome_prev_pnl_usdt NUMERIC;
ALTER TABLE autogrid_candidates ADD COLUMN IF NOT EXISTS outcome_revision INT DEFAULT 0;
