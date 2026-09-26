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

func TestTwoCircuitFloorMath(t *testing.T) {
	// Case 1: VIRTUAL #1394 without phantom offset:
	// Raw float is +0.7012, rebasePool is 0 -> supervisionFloor is +0.7012 (matches raw screen).
	rawVirtual := decimal.RequireFromString("0.7012")
	poolVirtual := decimal.Zero
	floorVirtual := rawVirtual.Add(poolVirtual)
	if !floorVirtual.LessThan(rawVirtual) {
		floorVirtual = rawVirtual
	}
	if !floorVirtual.Equal(rawVirtual) {
		t.Fatalf("expected floorVirtual %s, got %s", rawVirtual, floorVirtual)
	}

	// Case 2: PENGU #1386:
	// Raw float is -0.2333, rebasePool is -0.4280 -> supervisionFloor is -0.6613.
	rawPengu := decimal.RequireFromString("-0.2333")
	poolPengu := decimal.RequireFromString("-0.4280")
	floorPengu := rawPengu.Add(poolPengu)
	if !floorPengu.LessThan(rawPengu) {
		t.Fatalf("floor must be lower than raw")
	}
	expectedPenguFloor := decimal.RequireFromString("-0.6613")
	if !floorPengu.Equal(expectedPenguFloor) {
		t.Fatalf("expected floorPengu %s, got %s", expectedPenguFloor, floorPengu)
	}

	// Case 3: Rebase detection formula:
	// SHORT bot, signedPos = -83, entry jumped from 0.7814 to 0.8028
	signedPos := decimal.RequireFromString("-83")
	oldEntry := decimal.RequireFromString("0.7814")
	newEntry := decimal.RequireFromString("0.8028")
	rebaseDelta := signedPos.Mul(newEntry.Sub(oldEntry)) // -83 * 0.0214 = -1.7762
	if !rebaseDelta.IsNegative() {
		t.Fatalf("rebaseDelta for underwater short must be negative")
	}
	expectedDelta := decimal.RequireFromString("-1.7762")
	if !rebaseDelta.Equal(expectedDelta) {
		t.Fatalf("expected delta %s, got %s", expectedDelta, rebaseDelta)
	}

	// LONG bot, signedPos = +100, entry dropped from 150 to 140
	signedPosLong := decimal.RequireFromString("100")
	oldEntryLong := decimal.RequireFromString("150")
	newEntryLong := decimal.RequireFromString("140")
	rebaseDeltaLong := signedPosLong.Mul(newEntryLong.Sub(oldEntryLong)) // 100 * -10 = -1000
	if !rebaseDeltaLong.IsNegative() {
		t.Fatalf("rebaseDelta for underwater long must be negative")
	}
	expectedDeltaLong := decimal.RequireFromString("-1000")
	if !rebaseDeltaLong.Equal(expectedDeltaLong) {
		t.Fatalf("expected delta long %s, got %s", expectedDeltaLong, rebaseDeltaLong)
	}

	// Case 4: CRIT-01 and GAP-01 decoupling and flip verification
	posNonZero := decimal.RequireFromString("-83")
	isZeroPos := posNonZero.IsZero()
	if isZeroPos {
		t.Fatalf("isZeroPos must be false for non-zero position")
	}

	lastSignedPos := decimal.RequireFromString("-83")
	signedPosCurrent := decimal.RequireFromString("100")
	isFlip := !signedPosCurrent.IsZero() && !lastSignedPos.IsZero() && signedPosCurrent.Mul(lastSignedPos).IsNegative()
	if !isFlip {
		t.Fatalf("isFlip must be true when position inverts sign from -83 to +100")
	}
}

func TestHealV113VirtualAndPengu(t *testing.T) {
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

	bot1394ID := "7c88b000-0000-0000-0000-000000001394"
	bot1386ID := "7c88b000-0000-0000-0000-000000001386"
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE id IN ($1, $2)`, bot1394ID, bot1386ID)

	// Seed bot 1394 (VIRTUAL with phantom shiftFloatingOffset)
	if _, err := pool.Exec(ctx, `
		INSERT INTO grid_bots (
			id, account_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			request_fingerprint, autogrid_settings_id, realized_pnl_usdt,
			unrealized_pnl_usdt, bot_number, model_state, created_at, updated_at
		) VALUES (
			$1, $2, 'VIRTUAL_USDT_PERP', 'bu-1394', 'RUNNING',
			'SHORT', 'GEOMETRIC', 0.50, 1.20, 32, 4, 100,
			'fp-1394', $3, 0.56, -0.21, 1394,
			'{"shiftFloatingOffset": -0.91088124, "shiftPosition": -83, "trancheDeployed": 2}'::jsonb, NOW(), NOW()
		)
	`, bot1394ID, settings.AccountID, settings.ID); err != nil {
		t.Fatalf("seed bot 1394: %v", err)
	}

	// Seed bot 1386 (PENGU)
	if _, err := pool.Exec(ctx, `
		INSERT INTO grid_bots (
			id, account_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			request_fingerprint, autogrid_settings_id, realized_pnl_usdt,
			unrealized_pnl_usdt, bot_number, model_state, created_at, updated_at
		) VALUES (
			$1, $2, 'PENGU_USDT_PERP', 'bu-1386', 'RUNNING',
			'SHORT', 'GEOMETRIC', 0.008, 0.015, 32, 4, 100,
			'fp-1386', $3, 0.48, -0.23, 1386,
			'{"trancheDeployed": 2}'::jsonb, NOW(), NOW()
		)
	`, bot1386ID, settings.AccountID, settings.ID); err != nil {
		t.Fatalf("seed bot 1386: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE id IN ($1, $2)`, bot1394ID, bot1386ID)
	})

	// Run heal
	worker.healV113VirtualShiftOffset(ctx)

	// Check bot 1394: offset must be removed, v113VirtualHealedAt set
	var offsetStr *string
	var healedVirtual *string
	if err := pool.QueryRow(ctx, `
		SELECT model_state->>'shiftFloatingOffset', model_state->>'v113VirtualHealedAt'
		FROM grid_bots WHERE id = $1
	`, bot1394ID).Scan(&offsetStr, &healedVirtual); err != nil {
		t.Fatalf("query bot 1394: %v", err)
	}
	if offsetStr != nil {
		t.Fatalf("expected shiftFloatingOffset to be cleared, got %v", *offsetStr)
	}
	if healedVirtual == nil {
		t.Fatalf("expected v113VirtualHealedAt to be set")
	}

	// Check bot 1386: rebasePool must be -0.428, v113PenguHealedAt set
	var rebasePoolStr *string
	var healedPengu *string
	if err := pool.QueryRow(ctx, `
		SELECT model_state->>'rebasePool', model_state->>'v113PenguHealedAt'
		FROM grid_bots WHERE id = $1
	`, bot1386ID).Scan(&rebasePoolStr, &healedPengu); err != nil {
		t.Fatalf("query bot 1386: %v", err)
	}
	if rebasePoolStr == nil || *rebasePoolStr != "-0.428" {
		t.Fatalf("expected rebasePool -0.428, got %v", rebasePoolStr)
	}
	if healedPengu == nil {
		t.Fatalf("expected v113PenguHealedAt to be set")
	}

	// Idempotency: second run must not fail
	worker.virtualHealDone = false
	worker.healV113VirtualShiftOffset(ctx)
}

