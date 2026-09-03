package autogrid

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRadarCooldownDurableAcrossRestart pins the v2.0.68 durable cooldown:
// the last radar re-center is read from bot_execution_events (ADJUST_RANGE
// with a RADAR_* reason), so a worker restart no longer re-arms a fresh
// window. The regression: with the old in-memory map a fresh process
// allowed an instant second re-center even at dist 2σ; now an event 30
// minutes old blocks at 2σ and only a close-in knife (0.2σ → 15m window)
// becomes eligible again. Manage-loop ADJUST_RANGE events (RANGE_BREAK_*
// reasons) must never arm the window.
func TestRadarCooldownDurableAcrossRestart(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	const symbol = "RDR_CD_USDT_PERP"
	var botID string
	err = pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt
		) VALUES (
			$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 999, -999
		)
		RETURNING id
	`, settings.ID, symbol).Scan(&botID)
	if err != nil {
		t.Fatalf("insert paper bot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = $1`, botID); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE id = $1`, botID); err != nil {
			t.Errorf("cleanup paper bot: %v", err)
		}
	})

	// A radar re-center 30 minutes ago, plus a manage-loop range shift from
	// one minute ago: only the RADAR_* row may arm the cooldown.
	seed := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO bot_execution_events (
				bot_id, bot_number, bot_source, symbol,
				event_type, price, pnl_usdt, details, created_at
			) VALUES
				($1, 1, 'PAPER', $2, 'ADJUST_RANGE', 100, 0,
				 '{"reason":"RADAR_B3_RECENTER","action":"RADAR_RECENTER"}'::jsonb,
				 NOW() - INTERVAL '30 minutes'),
				($1, 1, 'PAPER', $2, 'ADJUST_RANGE', 100, 0,
				 '{"reason":"RANGE_BREAK_DOWN","new_lower":"80","new_upper":"100"}'::jsonb,
				 NOW() - INTERVAL '1 minute')
		`, botID, symbol); err != nil {
			t.Fatalf("seed events: %v", err)
		}
	}
	seed(t)

	// Fresh worker = post-restart state: no memory of the action.
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	lastAt, ok := worker.radarLastActionAt(ctx, botID)
	if !ok {
		t.Fatalf("radar re-center 30m ago must be found in bot_execution_events")
	}
	if age := time.Since(lastAt); age < 29*time.Minute || age > 31*time.Minute {
		t.Fatalf("last radar action must be ~30m old (not the 1m RANGE_BREAK row), got %v", age)
	}

	// The exact gating expression from radarMaybeRecenter.
	blocked := func(distToStopATR float64) bool {
		return ok && time.Since(lastAt) < radarActionCooldownFor(distToStopATR)
	}
	// dist 2σ → 2h window: a 30m-old re-center still blocks AFTER a restart.
	if !blocked(2.0) {
		t.Fatalf("dist 2σ with a re-center 30m ago must stay blocked across a restart")
	}
	// dist 0.2σ → 15m floor: the same 30m-old event no longer blocks — the
	// knife case the flat 2h window answered hours too late.
	if blocked(0.2) {
		t.Fatalf("dist 0.2σ with a re-center 30m ago must be allowed (15m window)")
	}

	// No radar event at all: fail-open, action eligible (old in-memory miss).
	if _, ok := worker.radarLastActionAt(ctx, "00000000-0000-0000-0000-000000000000"); ok {
		t.Fatalf("unknown bot must report no radar action")
	}
}
