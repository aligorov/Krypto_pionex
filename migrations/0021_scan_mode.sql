-- Migration 0021: scan mode toggle — TOP_K (fast, default) vs FULL (exhaustive).
ALTER TABLE autogrid_settings
    ADD COLUMN IF NOT EXISTS scan_mode VARCHAR(16) NOT NULL DEFAULT 'TOP_K';
