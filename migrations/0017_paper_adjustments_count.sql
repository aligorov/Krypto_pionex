-- Migration 0017: complete the paper-grid accounting schema.
--
-- Migration 0005 added adjustments_count to grid_bots but mirrored only
-- closed_reason to paper_grid_bots. The paper management loop selects and
-- increments paper_grid_bots.adjustments_count, so on any database built
-- purely from migrations the loop failed every cycle (SQLSTATE 42703) and
-- paper bots were never marked to market — no PnL updates at all.
ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS adjustments_count INT NOT NULL DEFAULT 0;
