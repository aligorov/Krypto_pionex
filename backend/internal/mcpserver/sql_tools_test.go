package mcpserver

import "testing"

func TestValidateAnalyticsSQL(t *testing.T) {
	ok := []string{
		"SELECT * FROM paper_grid_bots WHERE status = 'RUNNING'",
		"with x as (select symbol from autogrid_candidates) select * from x",
		"SELECT c.score, b.realized_pnl_usdt FROM autogrid_candidates c JOIN paper_grid_bots b ON b.candidate_id = c.id",
		"  SELECT COUNT(*) FROM bot_telemetry;  ",
	}
	for _, q := range ok {
		if _, err := validateAnalyticsSQL(q); err != nil {
			t.Fatalf("valid query rejected: %q -> %v", q, err)
		}
	}
	bad := map[string]string{
		"UPDATE paper_grid_bots SET status='X'":       "non-SELECT",
		"SELECT * INTO newtable FROM paper_grid_bots": "SELECT INTO",
		"SELECT 1 FROM paper_grid_bots; SELECT 2":     "multi-statement",
		"SELECT * FROM credential_keyring":            "secret table",
		"SELECT * FROM app_users":                     "users table",
		"SELECT * FROM pg_catalog.pg_tables":          "system catalog",
		"SELECT * FROM paper_grid_bots FOR UPDATE":    "FOR UPDATE",
		"SELECT pg_sleep(10)":                         "pg_sleep",
		"DELETE FROM paper_grid_bots":                 "DELETE",
		"select api_key from pionex_accounts":         "accounts table",
	}
	for q, why := range bad {
		if _, err := validateAnalyticsSQL(q); err == nil {
			t.Fatalf("%s must be rejected: %q", why, q)
		}
	}
}
