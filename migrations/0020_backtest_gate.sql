-- Migration 0020: walk-forward backtest gate for REAL deployments.
-- The gate requires a fresh OOS verdict on the traded timeframe and a
-- fragility check on neighbor timeframes; toggled via feature_flags
-- (Zero-ENV: lives in PostgreSQL, no settings-form round-trip hazard).
INSERT INTO feature_flags (name, enabled, description) VALUES
    ('backtest_gate', true,
     'Require a fresh walk-forward backtest verdict (traded TF pass + neighbor-TF fragility check) before REAL grid deployment')
ON CONFLICT (name) DO NOTHING;
