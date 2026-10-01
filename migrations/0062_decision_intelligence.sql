-- 0062 v2.0.184: Decision Intelligence module (docs/decision-analysis-plan-2026-10-01.md).
-- Observation, not participation: the trading path keeps its own policy;
-- analytics failures count as LOST observations in coverage, never block.
--
-- Three layers on top of the existing artifacts:
--   entry_decisions    -> gate trace, data passports, config snapshots, episodes
--   shadow_candidates  -> decision linkage, skip reasons, two state machines
--   replay_runs        -> immutable saved experiments (never overwrite)
--   gate_quality_daily -> per-regime aggregates with coverage and proof strength

-- ============ 1. entry_decisions extensions ============
ALTER TABLE entry_decisions
    -- Episode linkage: attempts of the same symbol+direction within the
    -- policy window collapse into ONE opportunity (a candidate rejected every
    -- 4 minutes must not inflate "missed profit").
    ADD COLUMN IF NOT EXISTS episode_id UUID,
    ADD COLUMN IF NOT EXISTS attempt_no INT NOT NULL DEFAULT 1,
    -- Data passport: when each indicator was measured vs when the decision
    -- was taken. observed_at/available_at per-indicator live in gate_trace;
    -- decision_at is the row-level stamp (created_at stays the insert time).
    ADD COLUMN IF NOT EXISTS decision_at TIMESTAMPTZ,
    -- Row-level rollup of indicator health at decision time:
    -- OK | MISSING | STALE | DESYNC | ERROR (RV=0 on a klines error is
    -- NEVER "no volatility").
    ADD COLUMN IF NOT EXISTS data_quality VARCHAR(16) NOT NULL DEFAULT 'OK',
    -- Ordered per-gate trace from the checks ALREADY executed (no extra
    -- exchange calls after a rejection):
    --   [{"gate":"RV","verdict":"PASS","threshold":1.5,"inputs":{"ratio":1.62},
    --     "observed_at":...,"available_at":...,"quality":"OK"},
    --    {"gate":"ANTI_FOMO","verdict":"EXEMPT","exemption":"confirmed_directional_trend",...},
    --    {"gate":"STRESS","verdict":"REJECT",...},
    --    {"gate":"KNIFE","verdict":"NOT_EVALUATED"},
    --    {"gate":"BACKTEST","verdict":"NOT_EVALUATED"}]
    -- verdicts: PASS | REJECT | WAIT | EXEMPT | NOT_EVALUATED | UNKNOWN
    ADD COLUMN IF NOT EXISTS gate_trace JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Geometry readiness at decision time: SCANNER | INTERMEDIATE | FINAL.
    -- Early rejections legitimately have no final geometry — it stays
    -- unknown instead of being simulated.
    ADD COLUMN IF NOT EXISTS stage VARCHAR(16) NOT NULL DEFAULT 'SCANNER',
    -- Values of the settings and feature flags that decided (a config hash
    -- identifies, it does not restore values).
    ADD COLUMN IF NOT EXISTS config_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Direction before/after the smart override.
    ADD COLUMN IF NOT EXISTS direction_before VARCHAR(16),
    ADD COLUMN IF NOT EXISTS direction_after VARCHAR(16),
    -- Price snapshot at decision time + its source (pionex mark | last | klines).
    ADD COLUMN IF NOT EXISTS price_at_decision NUMERIC(24,10),
    ADD COLUMN IF NOT EXISTS price_source VARCHAR(16);

CREATE INDEX IF NOT EXISTS idx_entry_decisions_episode
    ON entry_decisions (episode_id, attempt_no);
CREATE INDEX IF NOT EXISTS idx_entry_decisions_symbol_time
    ON entry_decisions (symbol, decision_at DESC);

-- Episodes: one row per linked entry opportunity.
CREATE TABLE IF NOT EXISTS entry_episodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol VARCHAR(32) NOT NULL,
    direction VARCHAR(16) NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Regime of the episode's scans (RANGE | TREND_UP | TREND_DOWN | UNKNOWN).
    regime VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN',
    attempts INT NOT NULL DEFAULT 1,
    -- Final disposition of the episode: ALLOWED | REJECTED_STILL | OPEN.
    disposition VARCHAR(16) NOT NULL DEFAULT 'OPEN'
);
CREATE INDEX IF NOT EXISTS idx_entry_episodes_symbol
    ON entry_episodes (symbol, last_seen DESC);

-- ============ 2. shadow_candidates extensions ============
ALTER TABLE shadow_candidates
    -- Linkage to the decision and episode that spawned the observation.
    ADD COLUMN IF NOT EXISTS decision_id UUID,
    ADD COLUMN IF NOT EXISTS episode_id UUID,
    -- Geometry stage captured (SCANNER mesh vs FINAL deploy mesh).
    ADD COLUMN IF NOT EXISTS geometry_stage VARCHAR(16) NOT NULL DEFAULT 'SCANNER',
    -- Why NO shadow exists for a rejection (denominator of coverage):
    -- CAP_REACHED | DUPLICATE_EPISODE | INVALID_GEOMETRY | DATA_QUALITY
    ADD COLUMN IF NOT EXISTS skip_reason VARCHAR(24),
    -- Position state machine (what happened to the virtual position):
    -- OPEN | CLOSED_TP | CLOSED_SL | HORIZON_END | INVALIDATED
    ADD COLUMN IF NOT EXISTS pos_state VARCHAR(16) NOT NULL DEFAULT 'OPEN',
    -- Calculation state machine (did we manage to compute it):
    -- PENDING | RUNNING | DONE | RETRYABLE_ERROR | INSUFFICIENT_DATA
    ADD COLUMN IF NOT EXISTS calc_state VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    -- Idempotent close bookkeeping: closed once, never re-accrues.
    ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ,
    -- Worker lease for crash-safe resumption.
    ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claimed_by VARCHAR(64),
    -- PnL decomposition (stored separately, never as one blob):
    -- {"grid":..,"inventory":..,"fees":..,"funding":..,"slippage":..}
    ADD COLUMN IF NOT EXISTS pnl_parts JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Directional mark-to-market proxy, explicitly NOT the gate's "$":
    -- which components it covers is listed in pnl_parts.proxy_covers.
    ADD COLUMN IF NOT EXISTS directional_mtm_proxy NUMERIC(14,6),
    -- Trajectory quality contract (gaps, source, ambiguity).
    -- {"price_source":"klines5m","observations":N,"max_gap_min":M,
    --  "tp_sl_same_candle":bool,"gap_clipped":bool}
    ADD COLUMN IF NOT EXISTS trajectory_quality JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Conservative vs alternative outcome when TP and SL hit the same candle.
    ADD COLUMN IF NOT EXISTS outcome_pnl_alt NUMERIC(14,6);

-- Coverage ledger: every rejection attempt either has a shadow or a skip
-- reason — the denominator the reports divide by.
CREATE TABLE IF NOT EXISTS shadow_coverage_log (
    id BIGSERIAL PRIMARY KEY,
    decision_id UUID NOT NULL,
    episode_id UUID,
    symbol VARCHAR(32) NOT NULL,
    scan_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    shadow_created BOOLEAN NOT NULL DEFAULT FALSE,
    skip_reason VARCHAR(24)
);
CREATE INDEX IF NOT EXISTS idx_shadow_coverage_time
    ON shadow_coverage_log (created_at DESC);

-- ============ 3. replay experiments (immutable) ============
CREATE TABLE IF NOT EXISTS replay_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by VARCHAR(64) NOT NULL DEFAULT 'operator',
    -- Window
    period_from TIMESTAMPTZ NOT NULL,
    period_to TIMESTAMPTZ NOT NULL,
    -- {"gate":"RV","overrides":{"threshold":1.8}} — empty = validation run
    -- (zero override MUST reproduce stored verdicts).
    baseline JSONB NOT NULL DEFAULT '{}'::jsonb,
    overrides JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Code/data/model versions for reproducibility.
    code_version TEXT NOT NULL DEFAULT '',
    data_versions JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- QUEUED | RUNNING | DONE | FAILED | CANCELLED
    status VARCHAR(16) NOT NULL DEFAULT 'QUEUED',
    status_reason TEXT NOT NULL DEFAULT '',
    -- Lease for the worker.
    lease_until TIMESTAMPTZ,
    -- Statistics: checked / verdicts_changed / full_chain_available /
    -- outcomes_available; plus not_replayable counts by cause.
    stats JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- The verdict-level effect (candidate-level, fixed sample):
    -- {"delta_pnl_proxy":..., "prevented_losses":..,"missed_profits":..}
    effect JSONB NOT NULL DEFAULT '{}'::jsonb,
    model_calibrated BOOLEAN,
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_replay_runs_created
    ON replay_runs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_replay_runs_status
    ON replay_runs (status, created_at DESC);

-- Per-decision replay verdicts (one row per checked decision).
CREATE TABLE IF NOT EXISTS replay_run_items (
    id BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES replay_runs(id) ON DELETE CASCADE,
    decision_id UUID NOT NULL,
    symbol VARCHAR(32) NOT NULL,
    -- REPRODUCED | CHANGED | NOT_REPLAYABLE
    verdict VARCHAR(16) NOT NULL,
    not_replayable_cause TEXT NOT NULL DEFAULT '',
    base_outcome VARCHAR(8) NOT NULL DEFAULT '',
    new_outcome VARCHAR(8) NOT NULL DEFAULT '',
    -- Was the WHOLE chain evaluated after the overridden gate passed?
    chain_followed BOOLEAN NOT NULL DEFAULT FALSE,
    -- Model outcome of the episode, when available.
    episode_outcome_pnl NUMERIC(14,6),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_replay_run_items_run
    ON replay_run_items (run_id, verdict);

-- ============ 4. per-regime gate quality aggregates ============
CREATE TABLE IF NOT EXISTS gate_quality_daily (
    id BIGSERIAL PRIMARY KEY,
    window_kind VARCHAR(8) NOT NULL,     -- 24H | 7D (WINDOW is a reserved PG word)
    window_start TIMESTAMPTZ NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    gate VARCHAR(32) NOT NULL,
    regime VARCHAR(16) NOT NULL,         -- RANGE | TREND_UP | TREND_DOWN | UNKNOWN
    episodes INT NOT NULL DEFAULT 0,
    decisions INT NOT NULL DEFAULT 0,
    -- Coverage: share of rejections that actually got a shadow observation.
    coverage NUMERIC(6,4) NOT NULL DEFAULT 0,
    skip_reasons JSONB NOT NULL DEFAULT '{}'::jsonb,
    completed_outcomes INT NOT NULL DEFAULT 0,
    open_outcomes INT NOT NULL DEFAULT 0,
    -- Model ±$ of blocked entries (per fixed investment), decomposed.
    blocked_model_pnl NUMERIC(14,6),
    blocked_model_pnl_parts JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Proof strength: LOW (thin coverage / open outcomes) | MEDIUM | HIGH.
    proof_strength VARCHAR(8) NOT NULL DEFAULT 'LOW',
    -- Model calibration on the ALLOW control group, by direction/regime.
    calibration JSONB NOT NULL DEFAULT '{}'::jsonb,
    notes JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_gate_quality_unique
    ON gate_quality_daily (window_kind, window_start, gate, regime);

-- ============ 5. retention ============
-- Raw traces 14 days, episodes 90 days, aggregates forever.
-- (Existing housekeeping worker picks these up via its retention sweep.)

-- ============ 6. feature flags ============
INSERT INTO feature_flags (name, enabled, description)
VALUES
    ('decision_intelligence', true,
     'v2.0.184: full gate-trace/passport/episode recording for the decision analysis module (observation only, never blocks the trading path)'),
    ('shadow_coverage_ledger', true,
     'v2.0.184: every rejection logs shadow_created or a skip reason into shadow_coverage_log (the coverage denominator)'),
    ('replay_experiments', true,
     'v2.0.184: immutable replay_runs experiments over stored decisions; zero-override runs must reproduce stored verdicts')
ON CONFLICT (name) DO NOTHING;
