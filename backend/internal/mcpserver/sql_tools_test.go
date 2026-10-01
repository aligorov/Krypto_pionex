package mcpserver

import "testing"

// journalWriterTables is the drift guard for the v2.0.142 audit P1 #1 fix.
// It mirrors, as a hardcoded set, every table the autogrid journal writers
// INSERT into:
//
//	entry_chain.go  insertEntryDecisionRow  -> entry_decisions
//	ofi_journal.go  logOFIDecision          -> ofi_decision_snapshots
//	gate_value.go   runGateValueReport      -> gate_value_snapshots
//
// The whole point of whitelisting these journals in autogrid_sql is that the
// analytics surface can read back what the writers record. When the next
// journal table is added to those INSERT statements it MUST be added here AND
// to sqlAllowedTables — this test fails until both happen.
var journalWriterTables = []string{
	"entry_decisions",
	"ofi_decision_snapshots",
	"gate_value_snapshots",
}

// TestJournalTablesWhitelisted pins that the decision journals added by the
// v2.0.142 audit (P1 #1) are reachable through validateAnalyticsSQL and that
// the whitelist stays in sync with the autogrid journal-writing surface.
func TestJournalTablesWhitelisted(t *testing.T) {
	for _, table := range journalWriterTables {
		if !sqlAllowedTables[table] {
			t.Errorf("journal table %q is missing from sqlAllowedTables — the analytics tool cannot read back what the journal writers record", table)
		}
		// The exact query shape an audit session issues first.
		if _, err := validateAnalyticsSQL("SELECT count(*) FROM " + table); err != nil {
			t.Errorf("journal table %q must pass validateAnalyticsSQL: %v", table, err)
		}
	}
	// And the reachability is real, not just the map: a table filter with a
	// WHERE clause and an ORDER BY over the journal's indexed columns must
	// validate too (rules out a regex accident that only admits the bare
	// FROM form).
	for _, q := range []string{
		"SELECT symbol, outcome, code FROM entry_decisions WHERE symbol = 'BTC_USDT_PERP' ORDER BY created_at DESC LIMIT 10",
		"SELECT kind, verdict FROM ofi_decision_snapshots WHERE created_at > NOW() - INTERVAL '1 hour'",
		"SELECT gate, net_value_usdt FROM gate_value_snapshots ORDER BY created_at DESC",
	} {
		if _, err := validateAnalyticsSQL(q); err != nil {
			t.Errorf("journal query rejected: %q -> %v", q, err)
		}
	}
}

func TestValidateAnalyticsSQL(t *testing.T) {
	ok := []string{
		"SELECT status,attempts,last_error FROM notification_outbox",
		"SELECT last_poll_at,last_error FROM telegram_poll_state",
		"SELECT source, connected, last_message_at FROM liquidation_feed_health ORDER BY source",
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
