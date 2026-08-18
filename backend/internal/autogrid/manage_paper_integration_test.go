package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestManagePaperBotsMarksToMarket pins the paper supervision loop's SELECT to
// its rows.Scan order. Regression guard for the v1.2.0 bug where anti-hunt
// support reordered the scan targets: every cycle died on the first row
// (NULL last_grid_level scanned into a non-pointer int) and paper bots were
// never marked to market — frozen PnL, no closes, no range shifts.
func TestManagePaperBotsMarksToMarket(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "MTM_USDT_PERP", "close": "95", "open": "100",
						"high": "101", "low": "94", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)

	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	const symbol = "MTM_USDT_PERP"
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
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE id = $1`, botID); err != nil {
			t.Errorf("cleanup paper bot: %v", err)
		}
	})

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	if err := worker.managePaperBots(ctx, *settings); err != nil {
		t.Fatalf("managePaperBots must mark bots to market without error, got: %v", err)
	}

	var markPrice, unrealized string
	var lastLevel *int
	if err := pool.QueryRow(ctx, `
		SELECT mark_price::TEXT, unrealized_pnl_usdt::TEXT, last_grid_level
		FROM paper_grid_bots WHERE id = $1
	`, botID).Scan(&markPrice, &unrealized, &lastLevel); err != nil {
		t.Fatalf("load paper bot after manage: %v", err)
	}
	if got, _ := strconv.ParseFloat(markPrice, 64); got != 95 {
		t.Fatalf("mark_price must follow the live tick 95, got %s", markPrice)
	}
	if unrealized == "0" || unrealized == "0.00000000" {
		t.Fatalf("price 95 is below the range midpoint 100 — unrealized PnL must be negative inventory mark, got %s", unrealized)
	}
	if lastLevel == nil || *lastLevel != 2 {
		t.Fatalf("last_grid_level must persist the current level 2 (95 in a 90–110/10 grid), got %v", lastLevel)
	}
}

// TestManagePaperBotsAccruesFunding pins the v1.3.22 funding leg: a neutral
// paper bot held 17h with leveraged inventory below the midpoint must settle
// two 8h funding boundaries (longs pay), persist the advanced anchor and log
// a FUNDING execution event.
func TestManagePaperBotsAccruesFunding(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "FND_USDT_PERP", "close": "95", "open": "100",
						"high": "101", "low": "94", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)

	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	const symbol = "FND_USDT_PERP"
	var botID string
	err = pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt, opened_at
		) VALUES (
			$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 999, -999, NOW() - INTERVAL '17 hours'
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

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	if err := worker.managePaperBots(ctx, *settings); err != nil {
		t.Fatalf("managePaperBots must accrue funding without error, got: %v", err)
	}

	// Inventory at 95: 2.5 levels × (200×2/10) = 100 notional, long → pays.
	// 17h held → 2 boundaries × 100 × 10bps = 0.2 USDT owed, no pair profit
	// on the first mark, so realized must be exactly -0.2.
	var realized string
	var lastFundingAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt::TEXT, last_funding_at
		FROM paper_grid_bots WHERE id = $1
	`, botID).Scan(&realized, &lastFundingAt); err != nil {
		t.Fatalf("load paper bot after funding accrual: %v", err)
	}
	if got, _ := strconv.ParseFloat(realized, 64); got > -0.199 || got < -0.201 {
		t.Fatalf("17h long inventory must owe two funding boundaries = -0.2, got %s", realized)
	}
	if lastFundingAt == nil {
		t.Fatalf("last_funding_at must be persisted after accrual")
	}
	if since := time.Since(*lastFundingAt); since < 30*time.Minute || since > 90*time.Minute {
		t.Fatalf("last_funding_at must advance to the last settled boundary (~1h ago), got %v old", since)
	}
	var events int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'FUNDING'
	`, botID).Scan(&events); err != nil {
		t.Fatalf("count funding events: %v", err)
	}
	if events != 1 {
		t.Fatalf("exactly one FUNDING event must be logged per accrual, got %d", events)
	}
}
