-- Official BLS 2026 release schedule (bls.gov/schedule/news_release/*):
-- the deterministic FUTURE calendar. FRED /releases/dates proved to be an
-- archive (past releases only — live-verified 2026-09-04), ForexFactory is
-- 429-blocked; these dates are published a year ahead and effectively fixed.
-- 8:30 AM ET = 12:30Z while EDT (through Oct 31), 13:30Z after Nov 1 (EST).
-- Sep–Nov dates confirmed verbatim from BLS schedule pages; December follows
-- the published pattern (first-Friday NFP, mid-month CPI/PPI, ±1–2d risk —
-- a wrong-day costs one 3h gate pause at the wrong time, nothing more).
INSERT INTO economic_events (event_time, country, impact, title, source)
VALUES
  ('2026-09-10 12:30:00+00', 'USD', 'High', 'Producer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-09-11 12:30:00+00', 'USD', 'High', 'Consumer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-10-02 12:30:00+00', 'USD', 'High', 'Employment Situation (BLS)', 'BLS_SCHED'),
  ('2026-10-14 12:30:00+00', 'USD', 'High', 'Consumer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-10-15 12:30:00+00', 'USD', 'High', 'Producer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-11-06 13:30:00+00', 'USD', 'High', 'Employment Situation (BLS)', 'BLS_SCHED'),
  ('2026-11-10 13:30:00+00', 'USD', 'High', 'Consumer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-11-13 13:30:00+00', 'USD', 'High', 'Producer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-12-04 13:30:00+00', 'USD', 'High', 'Employment Situation (BLS)', 'BLS_SCHED'),
  ('2026-12-15 13:30:00+00', 'USD', 'High', 'Consumer Price Index (BLS)', 'BLS_SCHED'),
  ('2026-12-17 13:30:00+00', 'USD', 'High', 'Producer Price Index (BLS)', 'BLS_SCHED')
ON CONFLICT DO NOTHING;
