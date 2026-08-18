-- Migration 0025: BRIN indexes for the smart-data retention deletes.
-- Since v2.0.3 the collector covers the full Pionex PERP universe (bulk
-- Binance/Bybit funding ~800 rows/min) and prunes old rows hourly in
-- bounded batches; the existing (symbol, captured_at) b-trees cannot serve
-- a captured_at-only DELETE efficiently, BRIN on captured_at can.

CREATE INDEX IF NOT EXISTS idx_funding_captured_brin ON funding_snapshots USING BRIN (captured_at);
CREATE INDEX IF NOT EXISTS idx_oi_captured_brin ON oi_history USING BRIN (captured_at);
CREATE INDEX IF NOT EXISTS idx_liquidation_captured_brin ON liquidation_events USING BRIN (captured_at);
