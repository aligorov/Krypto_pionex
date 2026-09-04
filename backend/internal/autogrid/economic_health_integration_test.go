package autogrid

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// economicIntegrationDatabaseURL gates tests that need a real PostgreSQL
// with the migrated schema (economic_events + fomc_meetings). The tests
// reset both tables — run against a disposable database only.
func economicIntegrationDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PIONEX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PIONEX_TEST_DATABASE_URL is not set; skipping integration test")
	}
	return url
}

// newEconomicTestWorker builds the minimal Worker dataHealthCheck needs.
func newEconomicTestWorker(pool *pgxpool.Pool) *Worker {
	return &Worker{
		db:          pool,
		logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dataAlarmAt: make(map[string]time.Time),
	}
}

// resetEconomicFixture empties economic_events and fomc_meetings (the gate
// also reads the hard-coded FOMC calendar — a meeting inside the window
// would masquerade as an economic-event block) and restores them after.
func resetEconomicFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM economic_events`); err != nil {
		t.Fatalf("reset economic_events: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM fomc_meetings`); err != nil {
		t.Fatalf("reset fomc_meetings: %v", err)
	}
	t.Cleanup(func() {
		// The stand DB is disposable; the hard-coded FOMC calendar from
		// migration 0032 is re-applied by re-running migrations if needed.
		_, _ = pool.Exec(context.Background(), `DELETE FROM economic_events`)
	})
}

// TestDataHealthCheckFREDCalendarAlive: a fresh FRED event silences the
// "эконом-гейт слеп" alarm even though ForexFactory is completely dead —
// the v2.0.86 semantics (FF 429 backoff alone must not page the operator).
func TestDataHealthCheckFREDCalendarAlive(t *testing.T) {
	pool, ctx := connectEconomicPool(t)
	resetEconomicFixture(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('Consumer Price Index', NOW() + INTERVAL '3 days', 'High', 'USD', 'FRED')
	`); err != nil {
		t.Fatalf("seed FRED event: %v", err)
	}
	worker := newEconomicTestWorker(pool)
	worker.dataHealthCheck(ctx)
	if _, alarmed := worker.dataAlarmAt["economic_events"]; alarmed {
		t.Fatal("fresh FRED events must keep the calendar alive (no economic_events alarm)")
	}
}

// TestDataHealthCheckFFOnlyStillCounts: a fresh ForexFactory row alone also
// keeps the calendar alive — FRED not being configured must not alarm while
// the fallback feed works.
func TestDataHealthCheckFFOnlyStillCounts(t *testing.T) {
	pool, ctx := connectEconomicPool(t)
	resetEconomicFixture(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('FF CPI m/m', NOW() + INTERVAL '2 days', 'High', 'USD', 'forexfactory')
	`); err != nil {
		t.Fatalf("seed FF event: %v", err)
	}
	worker := newEconomicTestWorker(pool)
	worker.dataHealthCheck(ctx)
	if _, alarmed := worker.dataAlarmAt["economic_events"]; alarmed {
		t.Fatal("fresh forexfactory events must keep the calendar alive (no economic_events alarm)")
	}
}

// TestDataHealthCheckStaleFFAlarms: FF-only rows OUTSIDE the +7d liveness
// window must trip the alarm — that is exactly the "FF dead, window buffer
// expired" production state that blinded the gate.
func TestDataHealthCheckStaleFFAlarms(t *testing.T) {
	pool, ctx := connectEconomicPool(t)
	resetEconomicFixture(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('FF CPI m/m', NOW() + INTERVAL '8 days', 'High', 'USD', 'forexfactory')
	`); err != nil {
		t.Fatalf("seed stale FF event: %v", err)
	}
	worker := newEconomicTestWorker(pool)
	worker.dataHealthCheck(ctx)
	if _, alarmed := worker.dataAlarmAt["economic_events"]; !alarmed {
		t.Fatal("stale FF-only calendar (nothing fresh within +7d) must raise the economic_events alarm")
	}
}

// TestDataHealthCheckEmptyAlarms: no events at all → alarm (both sources dead).
func TestDataHealthCheckEmptyAlarms(t *testing.T) {
	pool, ctx := connectEconomicPool(t)
	resetEconomicFixture(t, pool)
	worker := newEconomicTestWorker(pool)
	worker.dataHealthCheck(ctx)
	if _, alarmed := worker.dataAlarmAt["economic_events"]; !alarmed {
		t.Fatal("empty calendar must raise the economic_events alarm")
	}
}

// TestCheckEconomicEventsPicksUpFRED: the deploy gate (T-2h … T+1h block
// window, impact='High' only) must pick up FRED rows unchanged — and must
// NOT block on MEDIUM releases (same semantics FF High always had).
func TestCheckEconomicEventsPicksUpFRED(t *testing.T) {
	pool, ctx := connectEconomicPool(t)
	resetEconomicFixture(t, pool)
	worker := newEconomicTestWorker(pool)

	// HIGH inside the window: blocks, and the reason names the FRED title.
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('Consumer Price Index', NOW() + INTERVAL '90 minutes', 'High', 'USD', 'FRED')
	`); err != nil {
		t.Fatalf("seed FRED High event: %v", err)
	}
	blocked, reason := worker.CheckEconomicEvents(ctx, 2)
	if !blocked {
		t.Fatal("FRED High event 90m ahead must block deployments")
	}
	if reason != "Consumer Price Index" {
		t.Errorf("block reason = %q, want the FRED release title", reason)
	}

	// MEDIUM does not block — identical to the FF-era semantics.
	if _, err := pool.Exec(ctx, `DELETE FROM economic_events`); err != nil {
		t.Fatalf("clear events: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('Unemployment Insurance Weekly Claims', NOW() + INTERVAL '90 minutes', 'Medium', 'USD', 'FRED')
	`); err != nil {
		t.Fatalf("seed FRED Medium event: %v", err)
	}
	blocked, _ = worker.CheckEconomicEvents(ctx, 2)
	if blocked {
		t.Fatal("FRED Medium event must NOT block deployments (impact='High' only)")
	}

	// HIGH outside the ±window does not block either.
	if _, err := pool.Exec(ctx, `DELETE FROM economic_events`); err != nil {
		t.Fatalf("clear events: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('Employment Situation', NOW() + INTERVAL '5 hours', 'High', 'USD', 'FRED')
	`); err != nil {
		t.Fatalf("seed FRED far-future event: %v", err)
	}
	blocked, _ = worker.CheckEconomicEvents(ctx, 2)
	if blocked {
		t.Fatal("FRED High event 5h ahead is outside the T-2h..T+1h block window and must not block")
	}
}

func connectEconomicPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, economicIntegrationDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}
