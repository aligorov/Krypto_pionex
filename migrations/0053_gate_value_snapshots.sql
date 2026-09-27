-- Gate value snapshots (v2.0.138 package D): shadow_candidates (0031)
-- already replays top-scored REJECTED candidates through the paper-model
-- core and stores the counterfactual outcome (outcome_pnl_usdt), but
-- nothing aggregates it per gate — "did each gate prevent loss or block
-- profit" stayed unanswerable. This table stores one aggregated row per
-- normalized gate key per report run: samples attributed, average blocked
-- outcome, prevented loss (sum of negative blocked outcomes, positive
-- amount), missed profit (sum of positive blocked outcomes), net value
-- (prevented_loss − missed_profit; positive = the gate pays for itself)
-- and the win rate among blocked candidates.
--
-- Attribution note: compound rejection reasons ("; "-joined multi-gate
-- rejections) attribute the sample to EVERY matched gate, so per-gate
-- sample counts intentionally overlap — sum(samples) across gates can
-- exceed the shadow-row count in the window. details carries the window.

CREATE TABLE IF NOT EXISTS gate_value_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    gate VARCHAR(64) NOT NULL,        -- normalized gate key, e.g. OU_HALF_LIFE
    samples INT NOT NULL,             -- shadow rows attributed to this gate
    avg_outcome_pnl_usdt NUMERIC,
    prevented_loss_usdt NUMERIC,      -- sum of negative outcomes the gate blocked
    missed_profit_usdt NUMERIC,       -- sum of positive outcomes the gate blocked
    net_value_usdt NUMERIC,           -- prevented_loss + missed_profit (negative = gate pays)
    win_rate_blocked NUMERIC,         -- share of blocked outcomes that were profitable
    details JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_gate_value_created ON gate_value_snapshots (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_gate_value_gate ON gate_value_snapshots (gate, created_at DESC);
