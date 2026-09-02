-- Liquidation feed source (v2.0.62): Binance's public WS is data-blocked
-- for both networks this system runs on (2026-09-01/02: REST answers, stream
-- pushes zero bytes), leaving the cascade gate blind since launch. Bybit v5's
-- allLiquidation topic is the keyless full-flood replacement; Binance stays
-- selectable as a fallback via this config flip + restart.

INSERT INTO app_config (key, value, description) VALUES
    ('liquidation_source', '"bybit"', 'Liquidation WS source for the cascade gate: bybit (allLiquidation, default) | binance (!forceOrder@arr — geo-blocked on prod networks)')
ON CONFLICT (key) DO NOTHING;
