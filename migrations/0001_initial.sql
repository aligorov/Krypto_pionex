-- 0001_initial.sql: Initial Schema for Pionex Bot

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- 1. Accounts Configuration
CREATE TABLE pionex_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(64) NOT NULL,
    api_key_encrypted TEXT NOT NULL,
    api_secret_encrypted TEXT NOT NULL,
    is_enabled BOOLEAN NOT NULL DEFAULT false,
    is_paper BOOLEAN NOT NULL DEFAULT true,
    has_read_permission BOOLEAN NOT NULL DEFAULT false,
    has_futures_permission BOOLEAN NOT NULL DEFAULT false,
    has_bot_permission BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 2. Durable Risk Engine & Kill Switch Settings
CREATE TABLE risk_settings (
    id INT PRIMARY KEY DEFAULT 1,
    kill_switch_enabled BOOLEAN NOT NULL DEFAULT true,
    max_account_exposure_usd DECIMAL(18,8) NOT NULL DEFAULT 1000.0,
    max_symbol_exposure_usd DECIMAL(18,8) NOT NULL DEFAULT 300.0,
    max_daily_loss_usd DECIMAL(18,8) NOT NULL DEFAULT 50.0,
    max_leverage INT NOT NULL DEFAULT 10,
    max_active_grid_bots INT NOT NULL DEFAULT 3,
    max_open_positions INT NOT NULL DEFAULT 5,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO risk_settings (id, kill_switch_enabled) VALUES (1, true) ON CONFLICT DO NOTHING;

-- 3. Market Symbols & Data Cache
CREATE TABLE market_symbols (
    symbol VARCHAR(32) PRIMARY KEY,
    base_currency VARCHAR(16) NOT NULL,
    quote_currency VARCHAR(16) NOT NULL,
    symbol_type VARCHAR(16) NOT NULL DEFAULT 'PERP', -- SPOT, PERP
    min_amount DECIMAL(18,8) NOT NULL,
    price_precision INT NOT NULL,
    amount_precision INT NOT NULL,
    min_notional DECIMAL(18,8) NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT true,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 4. Native Pionex Futures Grid Bots Lifecycle
CREATE TABLE grid_bots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES pionex_accounts(id),
    symbol VARCHAR(32) NOT NULL,
    bu_order_id VARCHAR(128) UNIQUE, -- Remote Pionex buOrderId
    status VARCHAR(32) NOT NULL DEFAULT 'DRAFT', -- DRAFT, PENDING_SUBMISSION, SUBMITTED, RUNNING, STOPPING, STOPPED, FAILED
    direction VARCHAR(16) NOT NULL DEFAULT 'NEUTRAL', -- LONG, SHORT, NEUTRAL
    grid_type VARCHAR(16) NOT NULL DEFAULT 'ARITHMETIC', -- ARITHMETIC, GEOMETRIC
    lower_price DECIMAL(18,8) NOT NULL,
    upper_price DECIMAL(18,8) NOT NULL,
    grid_num INT NOT NULL,
    leverage INT NOT NULL DEFAULT 1,
    quote_investment DECIMAL(18,8) NOT NULL,
    extra_margin DECIMAL(18,8) NOT NULL DEFAULT 0.0,
    stop_loss DECIMAL(18,8),
    take_profit DECIMAL(18,8),
    request_fingerprint VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 5. Pattern Futures Orders
CREATE TABLE pattern_orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES pionex_accounts(id),
    symbol VARCHAR(32) NOT NULL,
    client_order_id VARCHAR(64) NOT NULL UNIQUE,
    pionex_order_id VARCHAR(64),
    pattern_type VARCHAR(32) NOT NULL, -- BOS, CHoCH, FVG, ORDER_BLOCK, etc.
    side VARCHAR(8) NOT NULL, -- BUY, SELL
    order_type VARCHAR(16) NOT NULL DEFAULT 'LIMIT', -- LIMIT, MARKET
    price DECIMAL(18,8) NOT NULL,
    quantity DECIMAL(18,8) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'CREATED', -- CREATED, SUBMITTED, PARTIALLY_FILLED, FILLED, CANCELLED, REJECTED
    stop_loss DECIMAL(18,8),
    take_profit DECIMAL(18,8),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 6. Telegram Transactional Outbox
CREATE TABLE notification_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type VARCHAR(64) NOT NULL,
    severity VARCHAR(16) NOT NULL DEFAULT 'INFO', -- CRITICAL, SECURITY, WARNING, SUCCESS, INFO, DEBUG
    payload JSONB NOT NULL,
    deduplication_key VARCHAR(128) UNIQUE,
    status VARCHAR(16) NOT NULL DEFAULT 'PENDING', -- PENDING, SENDING, SENT, FAILED
    attempts INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    scheduled_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 7. Audit Log
CREATE TABLE audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    action VARCHAR(64) NOT NULL,
    actor VARCHAR(64) NOT NULL,
    details JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
