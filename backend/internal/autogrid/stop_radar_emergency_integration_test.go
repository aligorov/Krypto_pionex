package autogrid

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRadarMicrostructureEmergencyExit_NeutralDgtDecoupled_Integration verifies:
// 1. NEUTRAL REAL bot with total = -$0.50 triggers emergency exit (STOP_REQUESTED)
//    without being blocked by the old -$1.00 DGT threshold.
// 2. DGT redeploy intent is queued in model_state.
func TestRadarMicrostructureEmergencyExit_NeutralDgtDecoupled_Integration(t *testing.T) {
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

	accountName := fmt.Sprintf("itest-em-dgt-%d", time.Now().UnixNano())
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "k", APISecret: "s",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.StopForecastMode = "ACTIVE"
	settings.DgtRedeployEnabled = true

	var botID string
	buOrderID := fmt.Sprintf("EM-%d", time.Now().UnixNano())
	err = pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, settings_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, realized_pnl_usdt, unrealized_pnl_usdt,
			bot_number, created_at, updated_at
		) VALUES (
			$1, $2, 'BTC_USDT_PERP', $3, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 250,
			100, 96, 0, -0.50,
			999, NOW(), NOW()
		)
		RETURNING id
	`, account.ID, settings.ID, buOrderID).Scan(&botID)
	if err != nil {
		t.Fatalf("insert real bot: %v", err)
	}

	b := radarInput{
		botID:         botID,
		botNumber:     999,
		botSource:     "REAL",
		symbol:        "BTC_USDT_PERP",
		direction:     "NEUTRAL",
		inventorySide: 1, // long inventory during confirmed dump
		ofiRegime:     "CONFIRMED_DUMP",
		ofiActionable: true,
		price:         d("96.0"),
		lower:         d("95.0"),
		upper:         d("105.0"),
		total:         d("-0.50"), // total is -$0.50 (would fail the old <= -$1.00 gate!)
	}

	fired := worker.radarMicrostructureEmergencyExit(ctx, *settings, b)
	if !fired {
		t.Fatalf("expected emergency exit to execute for NEUTRAL bot with total -$0.50")
	}

	// Verify status is STOP_REQUESTED
	var status, closedReason string
	var modelState map[string]any
	err = pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason, ''), model_state
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&status, &closedReason, &modelState)
	if err != nil {
		t.Fatalf("query bot: %v", err)
	}

	if status != "STOP_REQUESTED" {
		t.Fatalf("expected status STOP_REQUESTED, got %s", status)
	}
	if closedReason != "EMERGENCY_OFI_DUMP" {
		t.Fatalf("expected closed_reason EMERGENCY_OFI_DUMP, got %s", closedReason)
	}

	// Verify DGT re-deploy intent is queued in model_state
	if modelState["dgtRedeployPendingAt"] == nil {
		t.Fatalf("expected dgtRedeployPendingAt in model_state, got %v", modelState)
	}
	if modelState["dgtSlotBudget"] != "250" {
		t.Fatalf("expected dgtSlotBudget 250, got %v", modelState["dgtSlotBudget"])
	}
}

// TestRadarMicrostructureEmergencyExit_DBZeroRowsRetry_Integration verifies:
// When bot is no longer RUNNING (e.g. 0 rows affected by update),
// the emergency exit arms a 2s transient backoff debounce instead of 30s.
func TestRadarMicrostructureEmergencyExit_DBZeroRowsRetry_Integration(t *testing.T) {
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

	accountName := fmt.Sprintf("itest-em-retry-%d", time.Now().UnixNano())
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "k", APISecret: "s",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.StopForecastMode = "ACTIVE"

	// Bot is already STOPPED, so WHERE status = 'RUNNING' will affect 0 rows
	var botID string
	buOrderID := fmt.Sprintf("EM-ZERO-%d", time.Now().UnixNano())
	err = pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, settings_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, realized_pnl_usdt, unrealized_pnl_usdt,
			bot_number, created_at, updated_at
		) VALUES (
			$1, $2, 'BTC_USDT_PERP', $3, 'STOPPED', 'LONG', 'ARITHMETIC',
			90, 110, 10, 2, 250,
			100, 96, 0, -10.0,
			998, NOW(), NOW()
		)
		RETURNING id
	`, account.ID, settings.ID, buOrderID).Scan(&botID)
	if err != nil {
		t.Fatalf("insert real bot: %v", err)
	}

	b := radarInput{
		botID:         botID,
		botNumber:     998,
		botSource:     "REAL",
		symbol:        "BTC_USDT_PERP",
		direction:     "LONG",
		inventorySide: 1,
		ofiRegime:     "CONFIRMED_DUMP",
		ofiActionable: true,
		price:         d("96.0"),
		lower:         d("95.0"),
		upper:         d("105.0"),
		total:         d("-10.0"),
	}

	fired := worker.radarMicrostructureEmergencyExit(ctx, *settings, b)
	if fired {
		t.Fatalf("expected emergency exit to fail when 0 rows affected")
	}

	// Verify debounce is armed with 2s backoff, NOT 30s
	until := worker.emergencyExits[botID]
	remaining := time.Until(until)
	if remaining > 3*time.Second {
		t.Fatalf("0 rows affected should arm 2s backoff, got: %s", remaining)
	}
	if remaining <= 0 {
		t.Fatalf("expected non-zero remaining backoff")
	}
}
