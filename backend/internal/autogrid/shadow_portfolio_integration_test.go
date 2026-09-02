package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordingHandler captures structured log output so a failure the worker
// only logs — the production shadow-capture bug lived exclusively in docker
// logs for 13+ hours — surfaces directly in the test failure message.
type recordingHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.records = append(h.records, b.String())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) contains(fragment string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if strings.Contains(r, fragment) {
			return true
		}
	}
	return false
}

func (h *recordingHandler) joined() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.records, " | ")
}

// TestCaptureShadowCandidatesInsertsRejectedTops reproduces the production
// anomaly (2026-09-01): shadow_candidates stayed at 0 rows for 13+ hours
// with the flag on and SUCCEEDED scans every 150s, while the SELECT half of
// captureShadowCandidates' INSERT..SELECT returns rows when run manually.
// Seeds one scan with eight REJECTED candidates (seven scored, one NULL
// score — ranking is DESC NULLS LAST) plus one ACCEPTED, runs the capture
// and pins the top-5 landing.
func TestCaptureShadowCandidatesInsertsRejectedTops(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	rec := &recordingHandler{}
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	worker := NewWorker(pool, service, accounts.NewService(pool), riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	// The capture is flag-gated; migration 0031 seeds it on, but a shared DB
	// may have drifted — pin and restore.
	var savedFlag bool
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM feature_flags WHERE name = 'shadow_portfolio'), true)
	`).Scan(&savedFlag); err != nil {
		t.Fatalf("snapshot shadow flag: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			INSERT INTO feature_flags (name, enabled) VALUES ('shadow_portfolio', $1)
			ON CONFLICT (name) DO UPDATE SET enabled = EXCLUDED.enabled
		`, savedFlag); err != nil {
			t.Errorf("restore shadow flag: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO feature_flags (name, enabled) VALUES ('shadow_portfolio', true)
		ON CONFLICT (name) DO UPDATE SET enabled = TRUE
	`); err != nil {
		t.Fatalf("pin shadow flag on: %v", err)
	}

	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	// The scan-run delete cascades autogrid_candidates, whose own delete
	// cascades shadow_candidates — one cleanup anchor for the whole fixture.
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})

	// Eight REJECTED candidates: seven scored (90.5 down to 84.5) plus one
	// NULL score that must rank last, and one ACCEPTED that must never land.
	for i := 0; i < 8; i++ {
		score := fmt.Sprintf("%d.5", 90-i)
		if i == 7 {
			score = "NULL"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO autogrid_candidates (
				scan_id, symbol, decision, rejection_reason, score,
				current_price, lower_price, upper_price,
				grid_num, recommended_trend, model_assumptions
			) VALUES (
				$1, $2, 'REJECTED', 'gate: test rejection', `+score+`,
				100, 90, 110,
				10, 'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
			)
		`, scanID, fmt.Sprintf("SHDW%d_USDT_PERP", i)); err != nil {
			t.Fatalf("insert rejected candidate %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, score,
			current_price, lower_price, upper_price,
			grid_num, recommended_trend, model_assumptions
		) VALUES (
			$1, 'SHDWACC_USDT_PERP', 'ACCEPTED', 99.5,
			100, 90, 110,
			10, 'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		)
	`, scanID); err != nil {
		t.Fatalf("insert accepted candidate: %v", err)
	}

	worker.captureShadowCandidates(ctx, *settings, scanID)

	var captured int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM shadow_candidates WHERE scan_id = $1
	`, scanID).Scan(&captured); err != nil {
		t.Fatalf("count shadow rows: %v", err)
	}
	if captured != shadowTopZ {
		t.Fatalf("capture must land the top-%d rejected candidates, got %d rows; worker logs: %s",
			shadowTopZ, captured, rec.joined())
	}
	// The three lowest-ranked rejections (ranks 6-8: 85.5, 84.5, NULL) must
	// stay out.
	var lowRanked int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM shadow_candidates
		WHERE scan_id = $1 AND symbol IN ('SHDW5_USDT_PERP', 'SHDW6_USDT_PERP', 'SHDW7_USDT_PERP')
	`, scanID).Scan(&lowRanked); err != nil {
		t.Fatalf("count low-ranked shadow rows: %v", err)
	}
	if lowRanked != 0 {
		t.Fatalf("only the top-scored rejections may land, found %d low-ranked rows", lowRanked)
	}
	// Observability: every capture run must leave the eligible/inserted line.
	if !rec.contains("shadow capture: eligible=8") || !rec.contains("inserted=5") {
		t.Fatalf("capture must log eligible/inserted every run, logs: %s", rec.joined())
	}
}

// TestTransientSimFailure pins the transient/permanent split for shadow-row
// failures: restarts (context canceled/deadline) and transport blips
// (5xx, 429 cooldown, dial failures) must leave the row pending for the
// next batch; permanent rejections (4xx) and a gone source row consume it.
// Marking rows simulated on a restart is the bug that silently ate the
// counterfactual this module exists to collect.
func TestTransientSimFailure(t *testing.T) {
	transient := []error{
		fmt.Errorf("wrap: %w", context.Canceled),
		fmt.Errorf("wrap: %w", context.DeadlineExceeded),
		&pionex.APIError{StatusCode: 500, Message: "boom"},
		&pionex.APIError{StatusCode: 503, Message: "unavailable"},
		&pionex.APIError{StatusCode: 429, Message: "rate limited"},
		fmt.Errorf("pionex request failed: dial tcp: connection refused"),
	}
	for _, err := range transient {
		if !transientSimFailure(err) {
			t.Fatalf("error %v must classify as transient (row stays pending)", err)
		}
	}
	permanent := []error{
		nil,
		&pionex.APIError{StatusCode: 400, Message: "bad symbol"},
		&pionex.APIError{StatusCode: 404, Message: "not found"},
		pgx.ErrNoRows,
	}
	for _, err := range permanent {
		if transientSimFailure(err) {
			t.Fatalf("error %v must classify as permanent (row consumed)", err)
		}
	}
}

// shadowSimFixture pins the shadow flag on, pushes any too-recent
// simulated_at past the 20h spacing (the due-anchor is global — another
// test's fresh rows would gate this test's batch), and returns the worker
// plus its recording logger.
func shadowSimFixture(t *testing.T, pool *pgxpool.Pool) (*Worker, Settings, *recordingHandler) {
	t.Helper()
	ctx := context.Background()
	var savedFlag bool
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM feature_flags WHERE name = 'shadow_portfolio'), true)
	`).Scan(&savedFlag); err != nil {
		t.Fatalf("snapshot shadow flag: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			INSERT INTO feature_flags (name, enabled) VALUES ('shadow_portfolio', $1)
			ON CONFLICT (name) DO UPDATE SET enabled = EXCLUDED.enabled
		`, savedFlag); err != nil {
			t.Errorf("restore shadow flag: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO feature_flags (name, enabled) VALUES ('shadow_portfolio', true)
		ON CONFLICT (name) DO UPDATE SET enabled = TRUE
	`); err != nil {
		t.Fatalf("pin shadow flag on: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE shadow_candidates
		SET simulated_at = NOW() - INTERVAL '21 hours'
		WHERE simulated AND simulated_at > NOW() - INTERVAL '21 hours'
	`); err != nil {
		t.Fatalf("age simulated rows past the due spacing: %v", err)
	}
	rec := &recordingHandler{}
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	worker := NewWorker(pool, service, accounts.NewService(pool), riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	return worker, *settings, rec
}

// seedMaturedShadowRow lands one matured (captured 25h ago) pending shadow
// row tied to a fresh REJECTED candidate, cleaning up through the scan run.
func seedMaturedShadowRow(t *testing.T, pool *pgxpool.Pool, symbol string) {
	t.Helper()
	ctx := context.Background()
	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})
	var candidateID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, score, rejection_reason,
			current_price, lower_price, upper_price, grid_num,
			recommended_trend, model_assumptions
		) VALUES (
			$1, $2, 'REJECTED', 90.5, 'gate: test', 100, 90, 110, 10,
			'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		) RETURNING id
	`, scanID, symbol).Scan(&candidateID); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO shadow_candidates (
			candidate_id, scan_id, symbol, direction, score,
			mesh_lower, mesh_upper, grid_num, entry_price,
			leverage, investment, fee_bps, captured_at
		) VALUES (
			$1, $2, $3, 'NEUTRAL', 90.5,
			90, 110, 10, 100,
			2, 100, 15, NOW() - INTERVAL '25 hours'
		)
	`, candidateID, scanID, symbol); err != nil {
		t.Fatalf("insert shadow row: %v", err)
	}
}

// klinesStubServer serves 5M candles covering the [capturedAt, +24h]
// replay window of a row captured 25h ago: now−24h … now−5h.
func klinesStubServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/market/klines", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func shadowKlinesPayload() map[string]any {
	klines := make([]map[string]any, 0, 20)
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 20; i++ {
		candleTime := base.Add(time.Duration(i) * time.Hour)
		close := 100 + float64(i%5)*0.2
		klines = append(klines, map[string]any{
			"time":   candleTime.UnixMilli(),
			"open":   "100",
			"close":  fmt.Sprintf("%.2f", close),
			"high":   fmt.Sprintf("%.2f", close+0.5),
			"low":    "99",
			"volume": "10",
		})
	}
	return map[string]any{
		"result":    true,
		"timestamp": time.Now().UnixMilli(),
		"data":      map[string]any{"klines": klines},
	}
}

// TestShadowSimBootstrapBatchStartsOnEmptyAnchor pins the bootstrap fix:
// with NO simulated rows at all the due-anchor used to return ErrNoRows
// and early-return, so the first batch could never start (prod: 147
// pending / 0 simulated). With matured rows present the very next pass
// must select and simulate them — the anchor must not advance on an empty
// batch, or a freshly matured candidate would wait out the 20h spacing.
func TestShadowSimBootstrapBatchStartsOnEmptyAnchor(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, settings, _ := shadowSimFixture(t, pool)
	server := klinesStubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(shadowKlinesPayload())
	})
	worker.publicClient = pionex.NewClient(server.URL, "test-key", "test-secret")

	seedMaturedShadowRow(t, pool, "SHDWBOOT1_USDT_PERP")
	seedMaturedShadowRow(t, pool, "SHDWBOOT2_USDT_PERP")

	worker.shadowSimIfDue(ctx, settings)

	var simulated, pending int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE simulated), COUNT(*) FILTER (WHERE NOT simulated)
		FROM shadow_candidates WHERE symbol LIKE 'SHDWBOOT%_USDT_PERP'
	`).Scan(&simulated, &pending); err != nil {
		t.Fatalf("count shadow rows: %v", err)
	}
	if simulated != 2 || pending != 0 {
		t.Fatalf("bootstrap batch must simulate both matured rows, got %d simulated / %d pending", simulated, pending)
	}
	var candles int
	if err := pool.QueryRow(ctx, `
		SELECT candles_used FROM shadow_candidates WHERE symbol = 'SHDWBOOT1_USDT_PERP'
	`).Scan(&candles); err != nil || candles == 0 {
		t.Fatalf("replay must consume candles, got %d (%v)", candles, err)
	}
}

// TestShadowSimContextCancelStaysPending pins the transient-failure split:
// a context cancellation mid-batch (worker restart) must leave the row
// PENDING for the next run — the old fail() marked it simulated on any
// error, silently eating the counterfactual.
func TestShadowSimContextCancelStaysPending(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, settings, rec := shadowSimFixture(t, pool)
	simCtx, cancel := context.WithCancel(ctx)
	server := klinesStubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Kill the worker's context mid-batch, then fail the in-flight
		// request — the client aborts with a wrapped context.Canceled.
		cancel()
		http.Error(w, "shutting down", http.StatusInternalServerError)
	})
	worker.publicClient = pionex.NewClient(server.URL, "test-key", "test-secret")

	seedMaturedShadowRow(t, pool, "SHDWCXL_USDT_PERP")

	worker.shadowSimIfDue(simCtx, settings)

	var simulated bool
	var notes string
	if err := pool.QueryRow(ctx, `
		SELECT simulated, COALESCE(sim_notes::TEXT, '') FROM shadow_candidates
		WHERE symbol = 'SHDWCXL_USDT_PERP'
	`).Scan(&simulated, &notes); err != nil {
		t.Fatalf("load shadow row: %v", err)
	}
	if simulated {
		t.Fatalf("context-canceled row must stay pending, got simulated with notes %s", notes)
	}
	if strings.Contains(notes, "error") {
		t.Fatalf("transient failure must not consume the row with an error note, got %s", notes)
	}
	// The batch must have ATTEMPTED the row (bootstrap anchor worked) and
	// classified the failure transient — both are only visible in logs.
	if !rec.contains("row stays pending") {
		t.Fatalf("transient classification must log the pending retry, logs: %s", rec.joined())
	}
}
