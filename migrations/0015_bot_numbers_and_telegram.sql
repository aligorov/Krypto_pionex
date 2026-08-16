-- Migration 0015: Bot sequential numbering, execution event history, and configurable Telegram settings

CREATE SEQUENCE IF NOT EXISTS bot_number_seq START WITH 101 INCREMENT BY 1;

-- Add bot_number to paper_grid_bots
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'paper_grid_bots' AND column_name = 'bot_number'
    ) THEN
        ALTER TABLE paper_grid_bots ADD COLUMN bot_number INT NOT NULL DEFAULT nextval('bot_number_seq');
    END IF;
END $$;

-- Add bot_number to grid_bots
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'grid_bots' AND column_name = 'bot_number'
    ) THEN
        ALTER TABLE grid_bots ADD COLUMN bot_number INT NOT NULL DEFAULT nextval('bot_number_seq');
    END IF;
END $$;

-- Table for permanent lifecycle execution history of all bots
CREATE TABLE IF NOT EXISTS bot_execution_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id TEXT NOT NULL,
    bot_number INT NOT NULL,
    bot_source VARCHAR(16) NOT NULL DEFAULT 'PAPER', -- PAPER or REAL
    symbol VARCHAR(64) NOT NULL,
    event_type VARCHAR(32) NOT NULL, -- CREATED, GRID_FILL, ADJUST_RANGE, TAKE_PROFIT, STOP_LOSS, MANUAL_STOP, RECONCILE
    price NUMERIC(30, 10),
    pnl_usdt NUMERIC(30, 10),
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_bot_execution_events_bot_id ON bot_execution_events (bot_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_bot_execution_events_symbol ON bot_execution_events (symbol, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_bot_execution_events_created ON bot_execution_events (created_at DESC);

-- Table for Telegram notification settings and templates
CREATE TABLE IF NOT EXISTS telegram_settings (
    id INT PRIMARY KEY DEFAULT 1,
    enabled BOOLEAN NOT NULL DEFAULT false,
    bot_token TEXT NOT NULL DEFAULT '',
    chat_id TEXT NOT NULL DEFAULT '',
    topic_id TEXT NOT NULL DEFAULT '',
    notify_bot_created BOOLEAN NOT NULL DEFAULT true,
    notify_take_profit BOOLEAN NOT NULL DEFAULT true,
    notify_stop_loss BOOLEAN NOT NULL DEFAULT true,
    notify_range_adjust BOOLEAN NOT NULL DEFAULT true,
    notify_digest BOOLEAN NOT NULL DEFAULT true,
    notify_emergency BOOLEAN NOT NULL DEFAULT true,
    digest_interval_minutes INT NOT NULL DEFAULT 60,
    template_bot_created TEXT NOT NULL DEFAULT '🚀 <b>Бот #{{bot_number}} запущен</b>
Символ: <code>{{symbol}}</code>
Направление: <b>{{direction}}</b> | Плечо: <b>{{leverage}}x</b>
Диапазон: <code>{{lower_price}} – {{upper_price}}</code> ({{grid_num}} ур.)
Инвестиция: <b>{{quote_investment}} USDT</b>',
    template_take_profit TEXT NOT NULL DEFAULT '🎯 <b>Тейк-профит: Бот #{{bot_number}}</b>
Символ: <code>{{symbol}}</code>
Прибыль: <b>+{{pnl_usdt}} USDT</b> ({{pnl_pct}}%)
Причина: <i>{{reason}}</i>',
    template_stop_loss TEXT NOT NULL DEFAULT '🛡️ <b>Стоп-лосс: Бот #{{bot_number}}</b>
Символ: <code>{{symbol}}</code>
Результат: <b>{{pnl_usdt}} USDT</b>
Причина: <i>{{reason}}</i>',
    template_range_adjust TEXT NOT NULL DEFAULT '🔄 <b>Сдвиг диапазона: Бот #{{bot_number}}</b>
Символ: <code>{{symbol}}</code>
Новый диапазон: <code>{{lower_price}} – {{upper_price}}</code>
Сдвигов: <b>{{adjustments_count}}</b>',
    template_digest TEXT NOT NULL DEFAULT '📊 <b>Периодическая сводка AutoGrid</b>
Активных ботов: <b>{{active_bots}}</b>
Общий PnL: <b>{{total_pnl}} USDT</b>
Реализованный PnL: <b>{{realized_pnl}} USDT</b>
Баланс USDT: <b>{{balance_usdt}}</b>',
    last_digest_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT single_telegram_settings CHECK (id = 1)
);

INSERT INTO telegram_settings (id, enabled) VALUES (1, false)
ON CONFLICT (id) DO NOTHING;
