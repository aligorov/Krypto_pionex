-- Migration 0003: Pionex Accounts Metadata & Auto-Grid Autopilot Tables

-- 1. Extend pionex_accounts
ALTER TABLE pionex_accounts
    ADD COLUMN IF NOT EXISTS key_fingerprint VARCHAR(64),
    ADD COLUMN IF NOT EXISTS secret_version INT DEFAULT 1,
    ADD COLUMN IF NOT EXISTS last_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS capability_status VARCHAR(32) DEFAULT 'UNVERIFIED',
    ADD COLUMN IF NOT EXISTS last_error TEXT;

-- 2. Account Permission Health Table
CREATE TABLE IF NOT EXISTS account_permission_health (
    account_id UUID PRIMARY KEY REFERENCES pionex_accounts(id) ON DELETE CASCADE,
    can_read BOOLEAN NOT NULL DEFAULT false,
    can_trade BOOLEAN NOT NULL DEFAULT false,
    can_bot_trade BOOLEAN NOT NULL DEFAULT false,
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    error_message TEXT
);

-- 3. Auto-Grid Autopilot Settings Table
CREATE TABLE IF NOT EXISTS autogrid_settings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID REFERENCES pionex_accounts(id),
    status VARCHAR(32) NOT NULL DEFAULT 'STOPPED', -- STOPPED, STARTING, RUNNING, PAUSED, EMERGENCY_STOPPED
    execution_mode VARCHAR(16) NOT NULL DEFAULT 'PAPER', -- PAPER, REAL
    budget_usdt NUMERIC(20, 8) NOT NULL DEFAULT 1000.0,
    max_active_bots INT NOT NULL DEFAULT 3,
    leverage INT NOT NULL DEFAULT 5,
    min_sharpe NUMERIC(8, 4) DEFAULT 1.2,
    min_ev_pct NUMERIC(8, 4) DEFAULT 0.5,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 4. Auto-Grid Scan Runs & Candidates
CREATE TABLE IF NOT EXISTS autogrid_scan_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status VARCHAR(32) NOT NULL,
    candidates_found INT DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS autogrid_candidates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scan_id UUID REFERENCES autogrid_scan_runs(id) ON DELETE CASCADE,
    symbol VARCHAR(32) NOT NULL,
    volatility NUMERIC(10, 6),
    volume_24h NUMERIC(20, 8),
    funding_rate NUMERIC(10, 6),
    ev_pct NUMERIC(8, 4),
    sharpe NUMERIC(8, 4),
    decision VARCHAR(32) NOT NULL, -- ACCEPTED, REJECTED
    rejection_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 5. Extend control_commands for Worker Leasing & Retries
ALTER TABLE control_commands
    ADD COLUMN IF NOT EXISTS lease_owner VARCHAR(64),
    ADD COLUMN IF NOT EXISTS lease_expiry TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_retry TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dead_letter_status VARCHAR(32);

-- Index for worker command acquisition
CREATE INDEX IF NOT EXISTS idx_control_commands_queue ON control_commands (status, created_at DESC);
