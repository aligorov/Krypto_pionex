-- 0002_control_plane.sql
-- Durable users, sessions, configuration, observability and MCP control plane.

CREATE TABLE app_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username VARCHAR(64) NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    email VARCHAR(254),
    password_hash TEXT NOT NULL,
    role VARCHAR(16) NOT NULL CHECK (role IN ('VIEWER', 'OPERATOR', 'ADMIN')),
    is_active BOOLEAN NOT NULL DEFAULT true,
    must_change_password BOOLEAN NOT NULL DEFAULT true,
    failed_login_attempts INT NOT NULL DEFAULT 0 CHECK (failed_login_attempts >= 0),
    locked_until TIMESTAMPTZ,
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT app_users_username_normalized CHECK (username = lower(username)),
    CONSTRAINT app_users_username_format CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,63}$')
);

CREATE UNIQUE INDEX app_users_username_unique ON app_users (lower(username));
CREATE UNIQUE INDEX app_users_email_unique ON app_users (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE user_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    token_hash CHAR(64) NOT NULL UNIQUE,
    csrf_hash CHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ip_address INET,
    user_agent VARCHAR(512),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX user_sessions_user_active_idx
    ON user_sessions (user_id, expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE user_settings (
    user_id UUID PRIMARY KEY REFERENCES app_users(id) ON DELETE CASCADE,
    language VARCHAR(8) NOT NULL DEFAULT 'ru' CHECK (language IN ('ru', 'en')),
    timezone VARCHAR(64) NOT NULL DEFAULT 'Europe/Moscow',
    theme VARCHAR(16) NOT NULL DEFAULT 'dark' CHECK (theme IN ('dark', 'light', 'system')),
    default_account_id UUID REFERENCES pionex_accounts(id) ON DELETE SET NULL,
    preferences JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE app_config (
    key VARCHAR(128) PRIMARY KEY,
    value JSONB NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_sensitive BOOLEAN NOT NULL DEFAULT false,
    updated_by UUID REFERENCES app_users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO app_config (key, value, description) VALUES
    ('real_pattern_execution_enabled', 'false', 'Allow real ordinary Futures Order execution'),
    ('real_grid_execution_enabled', 'false', 'Allow real native Futures Grid execution'),
    ('telegram_write_commands_enabled', 'false', 'Allow Telegram write commands'),
    ('mcp_write_enabled', 'true', 'Allow MCP control-plane mutations'),
    ('mcp_dangerous_confirmation_required', 'true', 'Require two-phase confirmation for dangerous MCP commands'),
    ('session_ttl_hours', '24', 'Web session lifetime'),
    ('log_retention_days', '30', 'Application log retention')
ON CONFLICT (key) DO NOTHING;

CREATE TABLE feature_flags (
    name VARCHAR(128) PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT false,
    description TEXT NOT NULL DEFAULT '',
    updated_by UUID REFERENCES app_users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO feature_flags (name, enabled, description) VALUES
    ('frontend_control_plane', true, 'Enable authenticated operations dashboard'),
    ('mcp_control_plane', true, 'Enable Model Context Protocol tools'),
    ('paper_trading', true, 'Enable paper-trading workflows'),
    ('real_pattern_execution', false, 'Enable real Futures order executor'),
    ('real_native_grid', false, 'Enable real native Pionex Futures Grid executor')
ON CONFLICT (name) DO NOTHING;

CREATE TABLE api_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL,
    token_prefix VARCHAR(20) NOT NULL,
    token_hash CHAR(64) NOT NULL UNIQUE,
    scopes TEXT[] NOT NULL DEFAULT ARRAY['mcp:read']::TEXT[],
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, name)
);

CREATE TABLE application_logs (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    level VARCHAR(12) NOT NULL CHECK (level IN ('DEBUG', 'INFO', 'WARN', 'ERROR')),
    component VARCHAR(64) NOT NULL,
    message TEXT NOT NULL,
    request_id VARCHAR(64),
    actor_id UUID REFERENCES app_users(id) ON DELETE SET NULL,
    account_id UUID REFERENCES pionex_accounts(id) ON DELETE SET NULL,
    symbol VARCHAR(32),
    aggregate_id VARCHAR(128),
    fields JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX application_logs_time_idx ON application_logs (occurred_at DESC);
CREATE INDEX application_logs_request_idx ON application_logs (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX application_logs_level_idx ON application_logs (level, occurred_at DESC);

ALTER TABLE audit_events
    ADD COLUMN actor_id UUID REFERENCES app_users(id) ON DELETE SET NULL,
    ADD COLUMN actor_type VARCHAR(16) NOT NULL DEFAULT 'SYSTEM'
        CHECK (actor_type IN ('USER', 'MCP', 'TELEGRAM', 'SYSTEM')),
    ADD COLUMN resource_type VARCHAR(64),
    ADD COLUMN resource_id VARCHAR(128),
    ADD COLUMN outcome VARCHAR(16) NOT NULL DEFAULT 'SUCCESS'
        CHECK (outcome IN ('SUCCESS', 'DENIED', 'FAILED', 'PENDING')),
    ADD COLUMN request_id VARCHAR(64),
    ADD COLUMN ip_address INET,
    ADD COLUMN user_agent VARCHAR(512);

CREATE INDEX audit_events_created_idx ON audit_events (created_at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, created_at DESC);
CREATE INDEX audit_events_resource_idx ON audit_events (resource_type, resource_id, created_at DESC);

CREATE TABLE control_commands (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id UUID REFERENCES app_users(id) ON DELETE SET NULL,
    actor_type VARCHAR(16) NOT NULL CHECK (actor_type IN ('USER', 'MCP', 'TELEGRAM', 'SYSTEM')),
    command_type VARCHAR(64) NOT NULL,
    resource_type VARCHAR(64) NOT NULL,
    resource_id VARCHAR(128),
    arguments JSONB NOT NULL DEFAULT '{}'::jsonb,
    sanitized_arguments JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key VARCHAR(128) NOT NULL UNIQUE,
    status VARCHAR(32) NOT NULL CHECK (status IN (
        'PREPARED', 'CONFIRMATION_REQUIRED', 'QUEUED', 'EXECUTING',
        'SUCCEEDED', 'FAILED', 'DENIED', 'EXPIRED'
    )),
    confirmation_hash CHAR(64),
    confirmation_expires_at TIMESTAMPTZ,
    risk_result JSONB NOT NULL DEFAULT '{}'::jsonb,
    result JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ,
    executed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX control_commands_status_idx ON control_commands (status, created_at DESC);
CREATE INDEX control_commands_actor_idx ON control_commands (actor_id, created_at DESC);

CREATE TABLE system_incidents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_key VARCHAR(128) NOT NULL UNIQUE,
    severity VARCHAR(16) NOT NULL CHECK (severity IN ('CRITICAL', 'SECURITY', 'WARNING', 'INFO')),
    status VARCHAR(16) NOT NULL CHECK (status IN ('OPEN', 'ACKNOWLEDGED', 'RECOVERED', 'CLOSED')),
    title VARCHAR(256) NOT NULL,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recovered_at TIMESTAMPTZ,
    acknowledged_by UUID REFERENCES app_users(id) ON DELETE SET NULL
);

CREATE INDEX system_incidents_status_idx ON system_incidents (status, severity, last_seen_at DESC);
