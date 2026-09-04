-- Economic calendar source tracking (v2.0.86): ForexFactory 429'd twice in
-- a row with 12h backoffs (prod 2026-09) and the deploy economic gate went
-- blind once the week buffer expired. The FRED releases calendar
-- (api.stlouisfed.org/fred/releases[/dates]) now feeds economic_events as a
-- primary USD calendar; ForexFactory stays as fallback. This column lets
-- dataHealthCheck tell "calendar alive (FRED)" from "calendar dead (both
-- sources stale)" so a dead FF feed alone no longer pages the operator.
--
-- Legacy rows all came from the ForexFactory collector, hence the default.
-- No UNIQUE(title, event_time) is added on purpose: the FF path de-dupes via
-- NOT EXISTS and legacy rows may already collide; the FRED path is
-- idempotent by transactional DELETE(source='FRED' window)+INSERT instead.

ALTER TABLE economic_events
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'forexfactory';

CREATE INDEX IF NOT EXISTS idx_economic_events_source_time
    ON economic_events (source, event_time);
