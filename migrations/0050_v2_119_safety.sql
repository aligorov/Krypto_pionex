-- Migration 0050 (v2.0.119): safety hardening born from the 2026-09-26/27 incidents.
--
-- 1. wick_shield_enabled flips to DEFAULT false and is explicitly disabled on
--    existing settings rows. NEAR #1401: the shield deferred the max-loss stop
--    TWICE (05:46:54 and 05:49:47, both past the $8 cap, judged by a LOWER wick
--    while the short inventory died on the upper move); the bot settled
--    −$14.895 on an $8 cap. The code now excludes ActionCloseStopLoss from the
--    shield entirely and keys the wick side off the signed position — the flag
--    governs only structural/range-break deferrals and starts OFF.
--
-- 2. supervision_floor_pnl_usdt loses its DEFAULT 0 on both tables and every
--    stored 0 becomes NULL. Migration 0048's backfill defeated every
--    COALESCE(supervision_floor_pnl_usdt, unrealized_pnl_usdt, 0) fallback:
--    the column was never NULL, rows without a live reconcile pass read as
--    floor=0, and the radar's unrealized leg silently vanished for exactly the
--    bots whose depth fetches kept failing — the optimistic direction
--    underwater. NULL restores the fallback; the reconcile loop rewrites real
--    floors on its first pass.

ALTER TABLE autogrid_settings
    ALTER COLUMN wick_shield_enabled SET DEFAULT false;

UPDATE autogrid_settings SET wick_shield_enabled = false WHERE wick_shield_enabled;

ALTER TABLE grid_bots
    ALTER COLUMN supervision_floor_pnl_usdt DROP DEFAULT;

UPDATE grid_bots
    SET supervision_floor_pnl_usdt = NULL
    WHERE supervision_floor_pnl_usdt = 0;

ALTER TABLE paper_grid_bots
    ALTER COLUMN supervision_floor_pnl_usdt DROP DEFAULT;

UPDATE paper_grid_bots
    SET supervision_floor_pnl_usdt = NULL
    WHERE supervision_floor_pnl_usdt = 0;
