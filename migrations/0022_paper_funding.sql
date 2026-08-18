-- Migration 0022: honest paper economics for the grid simulator (v1.3.22).
-- last_funding_at anchors the 8h funding accrual; NULL = accrue from opened_at.
ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS last_funding_at TIMESTAMPTZ;

-- Funding rate applied by the paper simulator per 8h boundary, in basis
-- points of the leveraged exposure (10 bps = 0.01%, the Pionex baseline).
-- Managed directly in PostgreSQL (config_set / SQL); deliberately not part
-- of the settings UPDATE payload so UI saves cannot clobber it.
ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS paper_funding_rate_bps NUMERIC(10, 2) NOT NULL DEFAULT 10;
