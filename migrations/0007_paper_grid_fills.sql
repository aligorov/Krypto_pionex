-- Migration 0007: Neutral grid fill simulation for paper bots

-- Tracks the grid level occupied by the price at the last simulation tick so
-- each manage cycle can count level crossings and accrue simulated grid
-- profit (realistic movement for NEUTRAL paper bots instead of a flat zero).
ALTER TABLE paper_grid_bots
    ADD COLUMN IF NOT EXISTS last_grid_level INT;
