-- v2.0.165: REAL-fleet outcome analytics. outcome_* on autogrid_candidates
-- has been written exclusively by PAPER close paths since v2.0.54; the fleet
-- went REAL-only on 2026-09-25 08:50Z and the writer went silent — entry
-- cohorts have had no outcomes since. grid_bots gets the candidate link the
-- paper table always had.
ALTER TABLE grid_bots ADD COLUMN IF NOT EXISTS candidate_id UUID REFERENCES autogrid_candidates(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_grid_bots_candidate_id ON grid_bots (candidate_id) WHERE candidate_id IS NOT NULL;
