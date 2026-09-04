package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fredIntegrationDatabaseURL gates tests that need a real PostgreSQL with
// the migrated schema (economic_events + macro_sources). Run against a
// disposable database: the test resets economic_events entirely.
func fredIntegrationDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PIONEX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PIONEX_TEST_DATABASE_URL is not set; skipping integration test")
	}
	return url
}

// TestFREDCalendarReplaceIdempotentIntegration proves the two properties the
// 6h job relies on: (1) two consecutive runs leave the same row set —
// DELETE+INSERT window is the idempotency mechanism — and (2) rows from
// other sources (forexfactory fallback) survive the FRED window replace.
func TestFREDCalendarReplaceIdempotentIntegration(t *testing.T) {
	dbURL := fredIntegrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Snapshot & reset: disposable DB only. The FRED key is restored too.
	var prevKey string
	if err := pool.QueryRow(ctx,
		`SELECT fred_api_key FROM macro_sources WHERE id = 1`).Scan(&prevKey); err != nil {
		t.Fatalf("read macro_sources: %v (is migration 0032 applied?)", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE macro_sources SET fred_api_key = $1 WHERE id = 1`, "integration-test-key"); err != nil {
		t.Fatalf("pin test FRED key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`UPDATE macro_sources SET fred_api_key = $1 WHERE id = 1`, prevKey)
		_, _ = pool.Exec(context.Background(), `DELETE FROM economic_events`)
	})
	if _, err := pool.Exec(ctx, `DELETE FROM economic_events`); err != nil {
		t.Fatalf("reset economic_events: %v", err)
	}
	// A fallback row that must survive every FRED replace.
	if _, err := pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('FF CPI m/m', NOW() + INTERVAL '3 days', 'High', 'USD', 'forexfactory')
	`); err != nil {
		t.Fatalf("seed forexfactory row: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case fredReleasesPath:
			_, _ = w.Write([]byte(`{"releases":[
				{"id":10,"name":"Consumer Price Index"},
				{"id":50,"name":"Employment Situation"},
				{"id":78,"name":"Unemployment Insurance Weekly Claims"},
				{"id":336,"name":"Federal Funds Effective Rate"}
			]}`))
		case fredReleaseDatesPath:
			today := time.Now().UTC().Format("2006-01-02")
			_, _ = w.Write([]byte(`{"release_dates":[
				{"release_id":10,"date":"` + shiftDate(today, 7) + `"},
				{"release_id":50,"date":"` + shiftDate(today, 1) + `"},
				{"release_id":78,"date":"` + shiftDate(today, 3) + `"},
				{"release_id":336,"date":"` + today + `"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := DefaultCollectorConfig([]string{"BTC_USDT_PERP"})
	cfg.FREDBaseURL = server.URL
	cfg.MinRequestInterval = 0
	collector := NewCollectorWithConfig(pool, cfg)

	collector.collectFREDCalendar(ctx) // first run
	rows := snapshotEconomicEvents(t, pool)
	if len(rows) != 4 { // 3 FRED (daily 336 dropped) + 1 forexfactory survivor
		t.Fatalf("after first run: %d rows, want 4: %+v", len(rows), rows)
	}
	fredRows := filterSource(rows, EconomicEventSourceFRED)
	if len(fredRows) != 3 {
		t.Fatalf("FRED rows = %d, want 3: %+v", len(fredRows), fredRows)
	}
	for _, row := range fredRows {
		if row.Country != "USD" {
			t.Errorf("%q: country = %q, want USD", row.Title, row.Country)
		}
		if row.Impact != "High" && row.Impact != "Medium" {
			t.Errorf("%q: impact = %q, want High|Medium", row.Title, row.Impact)
		}
		if row.HourUTC != 13 {
			t.Errorf("%q: event hour = %d, want 13 (time-of-day heuristic)", row.Title, row.HourUTC)
		}
	}
	claims := findByTitle(fredRows, "Unemployment Insurance Weekly Claims")
	if claims == nil || claims.Impact != "Medium" {
		t.Errorf("claims row = %+v, want Medium (MEDIUM stored, never blocking)", claims)
	}
	if findByTitle(fredRows, "Employment Situation") == nil ||
		findByTitle(fredRows, "Consumer Price Index") == nil {
		t.Fatalf("HIGH releases missing: %+v", fredRows)
	}

	collector.collectFREDCalendar(ctx) // second run: must be a no-op
	after := snapshotEconomicEvents(t, pool)
	if len(after) != len(rows) {
		t.Fatalf("second run changed row count: %d → %d (idempotency broken)", len(rows), len(after))
	}
	for i := range rows {
		if rows[i] != after[i] {
			t.Errorf("row %d changed between runs: %+v → %+v", i, rows[i], after[i])
		}
	}
}

// economicEventRow is the comparable projection of one calendar row.
type economicEventRow struct {
	Title    string
	EventDay string // date only — captured_at and id may legitimately change
	HourUTC  int
	Impact   string
	Country  string
	Source   string
}

func snapshotEconomicEvents(t *testing.T, pool *pgxpool.Pool) []economicEventRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT title, event_time::DATE::TEXT,
		       EXTRACT(HOUR FROM event_time)::INT, impact, COALESCE(country, ''), COALESCE(source, '')
		FROM economic_events ORDER BY title, event_time
	`)
	if err != nil {
		t.Fatalf("snapshot economic_events: %v", err)
	}
	defer rows.Close()
	var out []economicEventRow
	for rows.Next() {
		var row economicEventRow
		if err := rows.Scan(&row.Title, &row.EventDay, &row.HourUTC, &row.Impact, &row.Country, &row.Source); err != nil {
			t.Fatalf("scan economic_events: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate economic_events: %v", err)
	}
	return out
}

func filterSource(rows []economicEventRow, source string) []economicEventRow {
	var out []economicEventRow
	for _, row := range rows {
		if row.Source == source {
			out = append(out, row)
		}
	}
	return out
}

func findByTitle(rows []economicEventRow, title string) *economicEventRow {
	for i := range rows {
		if rows[i].Title == title {
			return &rows[i]
		}
	}
	return nil
}

func shiftDate(dateOnly string, days int) string {
	day, err := time.Parse("2006-01-02", dateOnly)
	if err != nil {
		return dateOnly
	}
	return day.AddDate(0, 0, days).Format("2006-01-02")
}
