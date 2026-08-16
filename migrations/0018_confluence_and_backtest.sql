-- Migration 0018: confluence context on bots, backtest job queue, LLM
-- fail-closed option (confluence engine release 1, 2026-08-16).

-- Deploy-time structure/confluence snapshot the supervision loop needs to
-- validate that the market thesis a bot was opened under still holds.
-- (anti_hunt_stop_price exists since 0013; 0018 starts persisting it on
-- real bots too.)
ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS struct_context JSONB,
    ADD COLUMN IF NOT EXISTS struct_updated_at TIMESTAMPTZ;

ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS struct_context JSONB;

-- Walk-forward backtest job queue for the quant worker.
CREATE TABLE IF NOT EXISTS backtest_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol VARCHAR(64) NOT NULL,
    interval VARCHAR(8) NOT NULL DEFAULT '60M',
    status VARCHAR(16) NOT NULL DEFAULT 'QUEUED', -- QUEUED, RUNNING, DONE, FAILED
    params JSONB NOT NULL DEFAULT '{}',
    result JSONB,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_backtest_jobs_status ON backtest_jobs (status, created_at);

-- When TRUE, a REAL-mode candidate without a completed LLM audit is
-- rejected instead of passing unchecked (transport failures block entry).
ALTER TABLE llm_settings
    ADD COLUMN IF NOT EXISTS require_audit_for_real BOOLEAN NOT NULL DEFAULT FALSE;
