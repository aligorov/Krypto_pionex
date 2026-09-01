-- Shadow portfolio of rejected candidates (v2.0.56 F10): the 2026-09-01
-- checkpoint showed score does not predict outcomes (r = -0.01) and gates
-- have no counterfactual. This table captures the top-scored REJECTED
-- candidates per scan; a batch job replays them through the same pure
-- paper-model core (neutralGridPaperPNL / decideBotAction) over public
-- klines, so gate/score alpha becomes measurable instead of assumed.
-- Features are NOT duplicated — they live on the FK'd candidate row.

CREATE TABLE IF NOT EXISTS shadow_candidates (
    id                BIGSERIAL PRIMARY KEY,
    candidate_id      UUID NOT NULL REFERENCES autogrid_candidates(id) ON DELETE CASCADE,
    scan_id           UUID,
    symbol            VARCHAR(32) NOT NULL,
    score             NUMERIC(10,6),
    rejection_reason  TEXT,
    direction         VARCHAR(16) NOT NULL DEFAULT 'NEUTRAL',
    -- Scanner geometry (lower/upper/grid_num). The post-HAR deploy mesh is
    -- deliberately NOT captured: shadow measures the entry signal itself.
    mesh_lower        NUMERIC(24,10),
    mesh_upper        NUMERIC(24,10),
    grid_num          INT,
    entry_price       NUMERIC(24,10),
    -- Slot economics snapshot at capture time (settings drift protection).
    leverage          INT NOT NULL,
    investment        NUMERIC(20,8) NOT NULL,
    fee_bps           NUMERIC(10,4) NOT NULL DEFAULT 15,
    pnl_target_usdt   NUMERIC(14,6),
    max_loss_usdt     NUMERIC(14,6),
    captured_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    simulated         BOOLEAN NOT NULL DEFAULT FALSE,
    simulated_at      TIMESTAMPTZ,
    sim_window_start  TIMESTAMPTZ,
    sim_window_end    TIMESTAMPTZ,
    candles_used      INT,
    outcome_pnl_usdt  NUMERIC(14,6),
    outcome_reason    VARCHAR(48),
    mfe_usdt          NUMERIC(14,6),
    mae_usdt          NUMERIC(14,6),
    sim_notes         JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- One open shadow position per symbol; after simulated=TRUE the symbol is
-- capturable again — dedup without code.
CREATE UNIQUE INDEX IF NOT EXISTS idx_shadow_candidates_open_symbol
    ON shadow_candidates (symbol) WHERE NOT simulated;
CREATE INDEX IF NOT EXISTS idx_shadow_candidates_pending
    ON shadow_candidates (simulated, captured_at);

INSERT INTO feature_flags (name, enabled, description)
VALUES ('shadow_portfolio', true,
        'F10: capture top-scored rejected candidates and replay them through the paper-model core for gate/score alpha measurement')
ON CONFLICT (name) DO NOTHING;
