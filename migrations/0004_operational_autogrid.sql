-- 0004_operational_autogrid.sql
-- Durable Pionex credential storage, operational AutoGrid settings and paper execution.

CREATE TABLE IF NOT EXISTS credential_keyring (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    key_material BYTEA NOT NULL CHECK (octet_length(key_material) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS pionex_accounts_name_unique
    ON pionex_accounts (lower(name));

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS scope_key VARCHAR(32) NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS stop_loss_mode VARCHAR(24) NOT NULL DEFAULT 'ADAPTIVE_ATR',
    ADD COLUMN IF NOT EXISTS smart_pnl_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS adaptive_leverage_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS density_grid_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS candle_interval VARCHAR(8) NOT NULL DEFAULT '15M',
    ADD COLUMN IF NOT EXISTS lookback_candles INT NOT NULL DEFAULT 192,
    ADD COLUMN IF NOT EXISTS max_symbols_per_scan INT NOT NULL DEFAULT 12,
    ADD COLUMN IF NOT EXISTS scan_interval_seconds INT NOT NULL DEFAULT 300,
    ADD COLUMN IF NOT EXISTS min_volume_24h NUMERIC(24, 8) NOT NULL DEFAULT 1000000,
    ADD COLUMN IF NOT EXISTS min_volatility_pct NUMERIC(10, 6) NOT NULL DEFAULT 1.0,
    ADD COLUMN IF NOT EXISTS max_volatility_pct NUMERIC(10, 6) NOT NULL DEFAULT 20.0,
    ADD COLUMN IF NOT EXISTS max_drawdown_pct NUMERIC(10, 6) NOT NULL DEFAULT 15.0,
    ADD COLUMN IF NOT EXISTS min_profit_factor NUMERIC(10, 6) NOT NULL DEFAULT 1.05,
    ADD COLUMN IF NOT EXISTS fee_bps NUMERIC(10, 4) NOT NULL DEFAULT 10.0,
    ADD COLUMN IF NOT EXISTS slippage_bps NUMERIC(10, 4) NOT NULL DEFAULT 5.0,
    ADD COLUMN IF NOT EXISTS last_error TEXT,
    ADD COLUMN IF NOT EXISTS last_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_stopped_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE UNIQUE INDEX IF NOT EXISTS autogrid_settings_scope_unique
    ON autogrid_settings (scope_key);

ALTER TABLE autogrid_scan_runs
    ADD COLUMN IF NOT EXISTS settings_id UUID REFERENCES autogrid_settings(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS requested_by UUID REFERENCES app_users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS error_message TEXT;

ALTER TABLE autogrid_candidates
    ADD COLUMN IF NOT EXISTS current_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS score NUMERIC(10, 6),
    ADD COLUMN IF NOT EXISTS lower_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS upper_price NUMERIC(24, 10),
    ADD COLUMN IF NOT EXISTS grid_num INT,
    ADD COLUMN IF NOT EXISTS recommended_leverage INT,
    ADD COLUMN IF NOT EXISTS recommended_trend VARCHAR(16),
    ADD COLUMN IF NOT EXISTS max_drawdown_pct NUMERIC(10, 6),
    ADD COLUMN IF NOT EXISTS sortino NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS win_rate_pct NUMERIC(10, 6),
    ADD COLUMN IF NOT EXISTS profit_factor NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS turnover_proxy NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS model_assumptions JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS autogrid_candidates_scan_score_idx
    ON autogrid_candidates (scan_id, decision, score DESC);

ALTER TABLE grid_bots
    ADD COLUMN IF NOT EXISTS autogrid_settings_id UUID REFERENCES autogrid_settings(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS execution_mode VARCHAR(16) NOT NULL DEFAULT 'REAL',
    ADD COLUMN IF NOT EXISTS last_reconciled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reconciliation_state VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    ADD COLUMN IF NOT EXISTS last_remote_status VARCHAR(64),
    ADD COLUMN IF NOT EXISTS last_error TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS grid_bots_request_fingerprint_unique
    ON grid_bots (request_fingerprint);

CREATE TABLE IF NOT EXISTS paper_grid_bots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    settings_id UUID NOT NULL REFERENCES autogrid_settings(id) ON DELETE CASCADE,
    candidate_id UUID REFERENCES autogrid_candidates(id) ON DELETE SET NULL,
    symbol VARCHAR(32) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'RUNNING',
    direction VARCHAR(16) NOT NULL DEFAULT 'NEUTRAL',
    grid_type VARCHAR(16) NOT NULL DEFAULT 'ARITHMETIC',
    lower_price NUMERIC(24, 10) NOT NULL,
    upper_price NUMERIC(24, 10) NOT NULL,
    grid_num INT NOT NULL,
    leverage INT NOT NULL,
    quote_investment NUMERIC(20, 8) NOT NULL,
    entry_price NUMERIC(24, 10) NOT NULL,
    mark_price NUMERIC(24, 10) NOT NULL,
    realized_pnl_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0,
    unrealized_pnl_usdt NUMERIC(20, 8) NOT NULL DEFAULT 0,
    model_state JSONB NOT NULL DEFAULT '{}'::jsonb,
    opened_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS paper_grid_bots_one_active_symbol_idx
    ON paper_grid_bots (settings_id, symbol)
    WHERE status = 'RUNNING';

INSERT INTO autogrid_settings (
    scope_key, status, execution_mode, budget_usdt, max_active_bots,
    leverage, min_sharpe, min_ev_pct
) VALUES (
    'default', 'STOPPED', 'PAPER', 100, 1, 2, 0.25, 0.0
) ON CONFLICT (scope_key) DO NOTHING;
