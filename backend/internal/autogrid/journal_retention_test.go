package autogrid

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestJournalRetentionDeletesAgedRows (audit-2 P1 #2) pins the journal
// retention DELETEs against the real UUID-PK schema from migrations
// 0052/0053. The regression it guards: v2.0.138 shipped retention as
// `... AND id % 1000 = 0`, but these tables key on UUID and Postgres has no
// uuid % integer — every pass errored, the discarded error made retention
// dead code, and the journals grew without bound until v2.0.141 replaced the
// modulo with a plain created_at delete. Had this test existed, the very
// first nightly run would have failed with `operator does not exist:
// uuid % integer`.
//
// The three DELETE statements below are copied VERBATIM from the retention
// block in worker.go (the manage pass). Deliberately no worker internals are
// called: the point is to execute the exact production SQL, so a future edit
// that reintroduces a non-UUID-safe predicate (or drops a table from the
// block) turns this test red immediately.
func TestJournalRetentionDeletesAgedRows(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Unique markers so the test cannot collide with production journal rows
	// and can clean up everything it touched.
	const symbol = "JRET_PIN_USDT"
	const gate = "JRET_PIN_GATE"
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM entry_decisions WHERE symbol = $1`,
			`DELETE FROM ofi_decision_snapshots WHERE symbol = $1`,
		} {
			if _, err := pool.Exec(ctx, stmt, symbol); err != nil {
				t.Errorf("cleanup (%s): %v", stmt, err)
			}
		}
		if _, err := pool.Exec(ctx, `DELETE FROM gate_value_snapshots WHERE gate = $1`, gate); err != nil {
			t.Errorf("cleanup gate_value_snapshots: %v", err)
		}
	})

	seed := func(t *testing.T) {
		t.Helper()
		// entry_decisions: one aged row (20 days > the 14-day window) and
		// one fresh row. NOT NULL columns beyond created_at carry defaults.
		if _, err := pool.Exec(ctx, `
			INSERT INTO entry_decisions (path, fleet, symbol, outcome, code, created_at)
			VALUES
				('SCANNER_PAPER', 'PAPER', $1, 'ALLOW', 'JRET_PIN', NOW() - INTERVAL '20 days'),
				('SCANNER_PAPER', 'PAPER', $1, 'ALLOW', 'JRET_PIN', NOW())
		`, symbol); err != nil {
			t.Fatalf("seed entry_decisions: %v", err)
		}
		// ofi_decision_snapshots: same pair at 20 days / fresh.
		if _, err := pool.Exec(ctx, `
			INSERT INTO ofi_decision_snapshots (symbol, kind, regime, readiness, verdict, created_at)
			VALUES
				($1, 'ENTRY_VETO', 'BALANCED', 'READY', 'VETO', NOW() - INTERVAL '20 days'),
				($1, 'ENTRY_VETO', 'BALANCED', 'READY', 'VETO', NOW())
		`, symbol); err != nil {
			t.Fatalf("seed ofi_decision_snapshots: %v", err)
		}
		// gate_value_snapshots: the 90-day window of the third DELETE — one
		// aged row at 100 days and one fresh row.
		if _, err := pool.Exec(ctx, `
			INSERT INTO gate_value_snapshots (gate, samples, created_at)
			VALUES
				($1, 1, NOW() - INTERVAL '100 days'),
				($1, 1, NOW())
		`, gate); err != nil {
			t.Fatalf("seed gate_value_snapshots: %v", err)
		}
	}

	count := func(t *testing.T, table, where, arg string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+where, arg).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	seed(t)

	// The EXACT three statements from worker.go's retention block, copied
	// verbatim (worker.go, journal retention after the radar pass). Do not
	// "improve" them here — any divergence from production SQL defeats the
	// pin. Check the returned error: a silent `_, _ =` here is exactly how
	// the v2.0.141 bug survived a release.
	if _, err := pool.Exec(ctx, `DELETE FROM ofi_decision_snapshots WHERE created_at < NOW() - INTERVAL '14 days'`); err != nil {
		t.Fatalf("retention DELETE ofi_decision_snapshots failed (a predicate like `uuid %% 1000` would die exactly here): %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM entry_decisions WHERE created_at < NOW() - INTERVAL '14 days'`); err != nil {
		t.Fatalf("retention DELETE entry_decisions failed: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM gate_value_snapshots WHERE created_at < NOW() - INTERVAL '90 days'`); err != nil {
		t.Fatalf("retention DELETE gate_value_snapshots failed: %v", err)
	}

	// Aged rows gone, fresh rows survive — on all three journals.
	if n := count(t, "entry_decisions", "symbol = $1", symbol); n != 1 {
		t.Fatalf("entry_decisions: expected exactly the fresh row to survive, got %d", n)
	}
	if n := count(t, "ofi_decision_snapshots", "symbol = $1", symbol); n != 1 {
		t.Fatalf("ofi_decision_snapshots: expected exactly the fresh row to survive, got %d", n)
	}
	if n := count(t, "gate_value_snapshots", "gate = $1", gate); n != 1 {
		t.Fatalf("gate_value_snapshots: expected exactly the fresh row to survive, got %d", n)
	}
}
