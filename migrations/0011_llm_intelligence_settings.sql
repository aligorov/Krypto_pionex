-- Migration 0011: LLM Intelligence Settings and Candidate Audits

CREATE TABLE IF NOT EXISTS llm_settings (
    id INT PRIMARY KEY DEFAULT 1,
    enabled BOOLEAN NOT NULL DEFAULT false,
    provider VARCHAR(32) NOT NULL DEFAULT 'gemini',
    api_key TEXT NOT NULL DEFAULT '',
    model VARCHAR(128) NOT NULL DEFAULT 'gemini-2.0-flash',
    base_url TEXT NOT NULL DEFAULT '',
    temperature NUMERIC(3, 2) NOT NULL DEFAULT 0.20,
    thinking_enabled BOOLEAN NOT NULL DEFAULT true,
    require_approval_to_deploy BOOLEAN NOT NULL DEFAULT false,
    audit_interval_seconds INT NOT NULL DEFAULT 3600,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT single_row CHECK (id = 1)
);

INSERT INTO llm_settings (id, enabled, provider, model, temperature, thinking_enabled)
VALUES (1, false, 'gemini', 'gemini-2.0-flash', 0.20, true)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS llm_audits (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_id UUID REFERENCES autogrid_candidates(id) ON DELETE SET NULL,
    symbol VARCHAR(64) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    model VARCHAR(128) NOT NULL,
    decision VARCHAR(32) NOT NULL,
    confidence NUMERIC(4, 3) NOT NULL DEFAULT 0,
    regime VARCHAR(64) NOT NULL DEFAULT '',
    reasoning TEXT NOT NULL DEFAULT '',
    recommended_params JSONB NOT NULL DEFAULT '{}'::jsonb,
    raw_response TEXT NOT NULL DEFAULT '',
    latency_ms INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_llm_audits_symbol_created ON llm_audits (symbol, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_audits_decision ON llm_audits (decision);
