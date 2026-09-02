package autogrid

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
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
