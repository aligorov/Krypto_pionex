package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// TestShadowSimReplaysNewestFirstKlines pins the v2.0.75 root cause:
// /api/v1/market/klines returns candles NEWEST-FIRST, and the replay used to
// walk the raw order — the first (newest) candle always sat past the matured
// row's 24h window end, the After(windowEnd) break fired immediately and
// every simulation consumed ZERO candles (prod: 50 rows with candles_used=0,
// WINDOW_END, PnL 0, sim_window 0001-01-01). With the oldest-first sort the
// same newest-first feed must produce a real replay: candles consumed, a
// non-zero outcome, a materialized window.
func TestShadowSimReplaysNewestFirstKlines(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, settings, _ := shadowSimFixture(t, pool)

	// Row captured 25h ago; the replay window is [capturedAt, +24h]. 30
	// synthetic 5M candles AFTER capturedAt: 12 oscillation candles crossing
	// grid levels (completed pairs book realized profit), then a collapse to
	// 85.5 that loads the ladder and breaches the anti-hunt stop (88.5) —
	// the outcome is structurally non-zero (realized pairs survive the
	// breach; the breach branch itself marks no floating PnL).
	const symbol = "SHDWNF_USDT_PERP"
	seedMaturedShadowRow(t, pool, symbol)
	capturedAt := time.Now().UTC().Add(-25 * time.Hour)

	path := make([]float64, 0, 30)
	for i := 0; i < 12; i++ {
		if i%2 == 0 {
			path = append(path, 100)
		} else {
			path = append(path, 102)
		}
	}
	for i := 0; i < 18; i++ {
		path = append(path, 100-float64(i)*0.8)
	}

	candles := make([]map[string]any, 0, len(path))
	for i, close := range path {
		candleTime := capturedAt.Add(time.Duration(i+1) * 5 * time.Minute)
		candles = append(candles, map[string]any{
			"time":   candleTime.UnixMilli(),
			"open":   fmt.Sprintf("%.2f", close+0.1),
			"close":  fmt.Sprintf("%.2f", close),
			"high":   fmt.Sprintf("%.2f", close+0.2),
			"low":    fmt.Sprintf("%.2f", close-0.2),
			"volume": "10",
		})
	}
	// The EXCHANGE order: newest first. This is the payload shape that
	// produced the zero-candle replays.
	reversed := make([]map[string]any, 0, len(candles))
	for i := len(candles) - 1; i >= 0; i-- {
		reversed = append(reversed, candles[i])
	}
	server := klinesStubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data":      map[string]any{"klines": reversed},
		})
	})
	worker.publicClient = pionex.NewClient(server.URL, "test-key", "test-secret")

	worker.shadowSimIfDue(ctx, settings)

	var simulated bool
	var candlesUsed int
	var outcome string
	var reason string
	var windowStart *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT simulated, COALESCE(candles_used, 0), COALESCE(outcome_pnl_usdt::TEXT, '0'),
		       COALESCE(outcome_reason, ''), sim_window_start
		FROM shadow_candidates WHERE symbol = $1
	`, symbol).Scan(&simulated, &candlesUsed, &outcome, &reason, &windowStart); err != nil {
		t.Fatalf("load shadow row: %v", err)
	}
	if !simulated {
		t.Fatal("the row must be simulated after the batch")
	}
	if candlesUsed == 0 {
		t.Fatal("newest-first klines must still be replayed: candles_used > 0 required")
	}
	got := decimal.RequireFromString(outcome)
	if got.IsZero() {
		t.Fatalf("the synthetic collapse must produce a non-zero outcome, got %s (%s)", outcome, reason)
	}
	if windowStart == nil || windowStart.IsZero() {
		t.Fatal("the replay window start must be materialized (was 0001-01-01 in prod)")
	}
	if !reasonContainsBreach(reason) {
		t.Fatalf("the descent through the anti-hunt stop must settle via a close decision, got %q", reason)
	}
}

func reasonContainsBreach(reason string) bool {
	return reason != "" && reason != "WINDOW_END"
}
