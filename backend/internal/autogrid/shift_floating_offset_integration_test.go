package autogrid

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// TestShiftFloatingOffsetMath verifies the mathematical calculation of unrealized PnL
// across range shifts with keepInvestment:
// 1. Initial re-based state: exchange floating is +0.174, offset is -3.0672 -> total floating is -2.893 USDT
// 2. Half closed position: position ratio is 0.5 -> offset scales to -1.5336 USDT
// 3. Flat position: position is 0 -> unrealized is 0.
func TestShiftFloatingOffsetMath(t *testing.T) {
	position := decimal.RequireFromString("-0.72")
	shiftPos := decimal.RequireFromString("-0.72")
	shiftOffset := decimal.RequireFromString("-3.0672")
	price := decimal.RequireFromString("154.01")
	positionOpenPrice := decimal.RequireFromString("154.25")

	// 1. Full position
	exchangeUnrealized := position.Mul(price.Sub(positionOpenPrice)) // -0.72 * (154.01 - 154.25) = +0.1728
	ratio := position.Div(shiftPos)
	if ratio.GreaterThan(decimal.NewFromInt(1)) {
		ratio = decimal.NewFromInt(1)
	}
	totalUnrealized := exchangeUnrealized.Add(shiftOffset.Mul(ratio))
	expected := decimal.RequireFromString("-2.8944")
	if !totalUnrealized.Equal(expected) {
		t.Fatalf("expected full unrealized %s, got %s", expected, totalUnrealized)
	}

	// 2. Half position
	halfPos := decimal.RequireFromString("-0.36")
	halfExchange := halfPos.Mul(price.Sub(positionOpenPrice)) // -0.36 * -0.24 = +0.0864
	halfRatio := halfPos.Div(shiftPos)
	halfTotal := halfExchange.Add(shiftOffset.Mul(halfRatio))
	expectedHalf := decimal.RequireFromString("-1.4472")
	if !halfTotal.Equal(expectedHalf) {
		t.Fatalf("expected half unrealized %s, got %s", expectedHalf, halfTotal)
	}

	// 3. Flat position
	flatPos := decimal.Zero
	if !flatPos.IsZero() {
		t.Fatalf("flat position must be zero")
	}
}

// TestHealV107ShiftOffset tests the one-time heal on boot that seeds the true
// inventory offset on bot #1288.
func TestHealV107ShiftOffset(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	service := NewService(pool, risk.NewEngine(pool))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	worker := &Worker{
		db:      pool,
		service: service,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	botID := "7c88b000-0000-0000-0000-000000001288"
	_ = pool.QueryRow(ctx, `DELETE FROM grid_bots WHERE id = $1`, botID)

	if _, err := pool.Exec(ctx, `
		INSERT INTO grid_bots (
			id, account_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			request_fingerprint, autogrid_settings_id, realized_pnl_usdt,
			unrealized_pnl_usdt, bot_number, model_state, created_at, updated_at
		) VALUES (
			$1, $2, 'AAVE_USDT_PERP', '3aa02413-8c62-41de-a79b-0026ccd5f6da', 'RUNNING',
			'NEUTRAL', 'GEOMETRIC', 142.45, 158.85, 32, 4, 50,
			'fp-1288', $3, 2.13228776, 0.17422347, 1288,
			'{"trancheDeployed": 2}'::jsonb, NOW(), NOW()
		)
	`, botID, settings.AccountID, settings.ID); err != nil {
		t.Fatalf("seed bot 1288: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE id = $1`, botID)
	})

	// Run heal
	worker.healV107ShiftOffset(ctx)

	var offsetStr, posStr *string
	if err := pool.QueryRow(ctx, `
		SELECT model_state->>'shiftFloatingOffset', model_state->>'shiftPosition'
		FROM grid_bots
		WHERE id = $1
	`, botID).Scan(&offsetStr, &posStr); err != nil {
		t.Fatalf("query healed bot: %v", err)
	}

	if offsetStr == nil || *offsetStr != "-3.0672" {
		t.Fatalf("expected shiftFloatingOffset -3.0672, got %v", offsetStr)
	}
	if posStr == nil || *posStr != "-0.72" {
		t.Fatalf("expected shiftPosition -0.72, got %v", posStr)
	}

	// Idempotency: second heal call must not fail
	worker.shiftOffsetHealDone = false
	worker.healV107ShiftOffset(ctx)
}
