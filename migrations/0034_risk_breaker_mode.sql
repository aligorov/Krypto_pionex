-- Migration 0034: breaker derivation mode on risk_settings.
--
-- max_daily_loss_usd is a fleet-design DERIVATE (N × budget × leverage ×
-- ADAPTIVE_ATR stop floor × 1.25 headroom), not an operator free variable.
-- breaker_mode decides who owns the number:
--   AUTO   (default) — the autopilot re-derives it on every settings update
--                      and at worker start (drift self-heal).
--   MANUAL           — the operator pinned it explicitly via risk_update;
--                      derivation must not touch it.
-- The 0001 seed (max_daily_loss_usd 50) stays untouched: existing rows
-- inherit AUTO from the column default and the next derive reconciles them.

ALTER TABLE risk_settings
    ADD COLUMN IF NOT EXISTS breaker_mode VARCHAR(8) NOT NULL DEFAULT 'AUTO';

-- Constraint keeps the two-state contract durable; validated again in Go.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'risk_settings_breaker_mode_check'
    ) THEN
        ALTER TABLE risk_settings
            ADD CONSTRAINT risk_settings_breaker_mode_check
            CHECK (breaker_mode IN ('AUTO', 'MANUAL'));
    END IF;
END
$$;
