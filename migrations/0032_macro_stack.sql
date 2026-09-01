-- Macro context stack (v2.0.59): free-source regime data feeding the LLM
-- auditor and the deploy gates. Found by the 2026-09-01 data-gap audit:
-- the LLM evaluated candidates with zero market context, and FOMC decision
-- days were invisible (ForexFactory covers statistical releases only).
--
-- Sources (all keyless except FRED):
--   fred  : DGS2/DGS10/DTWEXBGS/VIXCLS/STLFSI4/T10YIE (key in macro_sources)
--   yahoo : ^VIX and DX-Y.NYB intraday (public chart API)
--   gnews : Google News RSS "Fed OR CPI OR FOMC when:6h" headlines
--   fomc  : hard-coded 2026-2027 calendars from federalreserve.gov
--           (updated 19.08.2026; official machine-readable format does not
--           exist — revise quarterly).

CREATE TABLE IF NOT EXISTS macro_snapshots (
    id BIGSERIAL PRIMARY KEY,
    source VARCHAR(32) NOT NULL,
    metric VARCHAR(32) NOT NULL,
    value NUMERIC(20,8) NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_macro_snapshots_metric_time
    ON macro_snapshots (metric, captured_at DESC);

CREATE TABLE IF NOT EXISTS news_headlines (
    id BIGSERIAL PRIMARY KEY,
    source VARCHAR(32) NOT NULL,
    title TEXT NOT NULL,
    url TEXT,
    published_at TIMESTAMPTZ,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source, title)
);

CREATE INDEX IF NOT EXISTS idx_news_headlines_time
    ON news_headlines (captured_at DESC);

CREATE TABLE IF NOT EXISTS fomc_meetings (
    id BIGSERIAL PRIMARY KEY,
    decision_at TIMESTAMPTZ NOT NULL, -- 14:00 ET on the second day (18:00Z EDT / 19:00Z EST)
    with_sep BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (decision_at)
);

INSERT INTO fomc_meetings (decision_at, with_sep) VALUES
    ('2026-01-28 19:00:00+00', false),
    ('2026-03-18 18:00:00+00', true),
    ('2026-04-29 18:00:00+00', false),
    ('2026-06-17 18:00:00+00', true),
    ('2026-07-29 18:00:00+00', false),
    ('2026-09-16 18:00:00+00', true),
    ('2026-10-28 18:00:00+00', false),
    ('2026-12-09 19:00:00+00', false),
    ('2027-01-27 19:00:00+00', false),
    ('2027-03-17 18:00:00+00', true),
    ('2027-04-28 18:00:00+00', false),
    ('2027-06-09 18:00:00+00', true),
    ('2027-07-28 18:00:00+00', false),
    ('2027-09-15 18:00:00+00', true),
    ('2027-10-27 18:00:00+00', false),
    ('2027-12-08 19:00:00+00', false)
ON CONFLICT (decision_at) DO NOTHING;

-- FRED credentials (Zero-ENV: the free api key lives here, not in env).
-- Empty key = the FRED collector leg skips silently; Yahoo/RSS/FOMC legs
-- are keyless and keep working.
CREATE TABLE IF NOT EXISTS macro_sources (
    id INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    fred_api_key TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO macro_sources (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
