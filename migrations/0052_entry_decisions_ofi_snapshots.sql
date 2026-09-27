-- 0052 v2.0.138: unified entry-decision journal (package B foundation) and
-- OFI decision snapshots (package VII data foundation).
--
-- entry_decisions: one write-only row per admission evaluation on EVERY path
-- that can open or add risk (scanner paper/REAL, DGT re-deploy, manual
-- deploy, invest_in, tranche-2). outcome is the machine-readable contract;
-- code names the deciding gate; features carries the feature vector at
-- decision time; config_version pins WHICH configuration decided.

CREATE TABLE IF NOT EXISTS entry_decisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    path VARCHAR(32) NOT NULL,      -- SCANNER_PAPER | SCANNER_REAL | DGT_REAL | DGT_PAPER | MANUAL_DEPLOY | INVEST_IN | TRANCHE2
    fleet VARCHAR(8) NOT NULL,      -- PAPER | REAL
    symbol VARCHAR(32) NOT NULL,
    outcome VARCHAR(8) NOT NULL,    -- ALLOW | WAIT | REJECT
    code VARCHAR(64) NOT NULL,      -- deciding gate, e.g. OFI_REENTRY_VETO
    reason TEXT NOT NULL DEFAULT '',
    features JSONB NOT NULL DEFAULT '{}'::jsonb,
    config_version TEXT NOT NULL DEFAULT '',
    ref_id TEXT                     -- candidate id / bot id / intent bot id
);

CREATE INDEX IF NOT EXISTS idx_entry_decisions_created
    ON entry_decisions (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entry_decisions_path_outcome
    ON entry_decisions (path, outcome, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entry_decisions_symbol
    ON entry_decisions (symbol, created_at DESC);

-- ofi_decision_snapshots: the microstructure feature vector at every
-- OFI-influenced decision (entry veto, radar pass, adjust freeze, re-entry
-- gate, emergency exit). The engine state is memory-only, so without these
-- rows the OFI-dependent decisions cannot be audited or replayed later.

CREATE TABLE IF NOT EXISTS ofi_decision_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    symbol VARCHAR(32) NOT NULL,
    kind VARCHAR(24) NOT NULL,          -- ENTRY_VETO | RADAR | ADJUST_FREEZE | REENTRY_GATE | EMERGENCY
    regime VARCHAR(24) NOT NULL,
    readiness VARCHAR(16) NOT NULL,     -- READY | WARMING_UP | STALE | DESYNC | RECOVERING
    ofi NUMERIC,
    taker_delta_usdt NUMERIC,
    micro_price_bias_bps NUMERIC,
    spread_bps NUMERIC,
    displacement_pct NUMERIC,
    windows_stable INT,
    verdict VARCHAR(16) NOT NULL,       -- ALLOW | VETO | FREEZE | SKIP | FIRED | BLOCKED
    veto_reason TEXT NOT NULL DEFAULT '',
    config_version TEXT NOT NULL DEFAULT '',
    ref_id TEXT                         -- candidate id / bot number / intent bot id
);

CREATE INDEX IF NOT EXISTS idx_ofi_decisions_created
    ON ofi_decision_snapshots (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ofi_decisions_symbol
    ON ofi_decision_snapshots (symbol, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ofi_decisions_kind
    ON ofi_decision_snapshots (kind, verdict, created_at DESC);
