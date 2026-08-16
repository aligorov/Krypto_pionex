-- Migration 0019: Gemini google_search grounding for the LLM news veto.
-- When enabled, the audit model performs a live web search for the token's
-- catalysts (unlocks, delists, exploits) instead of relying on training
-- data; the news_catalyst verdict becomes real-time.
ALTER TABLE llm_settings
    ADD COLUMN IF NOT EXISTS grounding_enabled BOOLEAN NOT NULL DEFAULT TRUE;
