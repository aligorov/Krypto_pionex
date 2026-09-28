-- Migration 0056: Configurable Margin Reserve Percentage in AutoGrid Settings.
-- Replaces rigid 30% hardcoded margin reserve with a configurable parameter (margin_reserve_pct).
-- Default is 0.00% (or operator chosen), allowing full capital utilization without artificial lockouts.

ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS margin_reserve_pct NUMERIC(5, 2) NOT NULL DEFAULT 0.00;

-- Update existing settings to 0.00% reserve so the operator can deploy immediately
UPDATE autogrid_settings
SET margin_reserve_pct = 0.00
WHERE margin_reserve_pct IS NULL;
