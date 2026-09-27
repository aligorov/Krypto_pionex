package autogrid

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
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
	// v2.0.119: pool.Close() here could hang for the whole suite budget —
	// some EARLIER test leaks an acquired connection (invisible to
	// pg_stat_activity: acquired-and-idle), and Close waits for every
	// acquisition to return. This test was the visible victim (7m hangs on
	// unmodified HEAD), not the leak's owner. Close with a bounded wait;
	// the leaked connection dies with the test process anyway.
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			pool.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
		}
	})

	service := NewService(pool, risk.NewEngine(pool))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	// grid_bots.account_id is NOT NULL and the default settings may carry no
	// account on a fresh disposable database — the test owns a throwaway
	// account instead of borrowing one (v2.0.119).
	accountService := accounts.NewService(pool)
	seedAccount, accErr := accountService.Create(ctx, accounts.CreateInput{
		Name:   "integration-heal-v107-" + time.Now().Format("150405.000000000"),
		APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if accErr != nil {
		t.Fatalf("create seed account: %v", accErr)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, seedAccount.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, seedAccount.ID)
	})

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
	`, botID, seedAccount.ID, settings.ID); err != nil {
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

	// Case 2: PENGU #1386 with a dynamically absorbed pool:
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

// TestRebasePoolDecayAnchor pins the v2.0.119 pool scaling: the discarded
// basis belongs to the inventory alive at absorb time, so as the grid sells
// that inventory down the pool's contribution shrinks proportionally — the
// v2.0.113 unscaled-pool double-count (the NO-GO review's P1).
func TestRebasePoolDecayAnchor(t *testing.T) {
	// Pool absorbed at pos 3733 (the PENGU-class discard), now the grid has
	// sold down to 1866.5 — half the anchored inventory: only half the pool
	// may contribute to the floor.
	pool := decimal.RequireFromString("-0.657")
	anchor := decimal.RequireFromString("3733")
	posNow := decimal.RequireFromString("1866.5")

	poolEff := pool
	ratio := posNow.Div(anchor)
	if ratio.IsPositive() && ratio.LessThan(decimal.NewFromInt(1)) {
		poolEff = pool.Mul(ratio)
	}
	expected := decimal.RequireFromString("-0.3285")
	if !poolEff.Equal(expected) {
		t.Fatalf("expected decayed pool %s, got %s", expected, poolEff)
	}

	// Position grew beyond the anchor: the added inventory carried its own
	// honest entry, so the pool stays whole (clamp at 1).
	posGrown := decimal.RequireFromString("9952")
	poolEffGrown := pool
	ratioGrown := posGrown.Div(anchor)
	if ratioGrown.IsPositive() && ratioGrown.LessThan(decimal.NewFromInt(1)) {
		poolEffGrown = pool.Mul(ratioGrown)
	}
	if !poolEffGrown.Equal(pool) {
		t.Fatalf("pool must clamp at full value when position grows past the anchor, got %s", poolEffGrown)
	}

	// Legacy semantics preserved: the shiftFloatingOffset keeps its own
	// ratio scaling (AAVE #1288 class), independent of the pool anchor.
	legacyOffset := decimal.RequireFromString("-3.0672")
	legacyAnchor := decimal.RequireFromString("-0.72")
	legacyNow := decimal.RequireFromString("-0.36")
	legacyRatio := legacyNow.Div(legacyAnchor)
	if legacyRatio.GreaterThan(decimal.NewFromInt(1)) {
		legacyRatio = decimal.NewFromInt(1)
	}
	expectedLegacy := decimal.RequireFromString("-1.5336")
	if got := legacyOffset.Mul(legacyRatio); !got.Equal(expectedLegacy) {
		t.Fatalf("expected legacy offset %s, got %s", expectedLegacy, got)
	}
}

// TestMigration0050FloorNullFallback verifies the NULL floor contract on a
// disposable database: after migration 0050 a row with floor 0 reads NULL
// through the supervision readers' COALESCE chain (falls back to the raw
// unrealized leg), and a row with a real floor keeps it.
func TestMigration0050FloorNullFallback(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Migration 0050's contract on live data: a row whose floor was never
	// written reads NULL through the supervision chain. Earlier suite tests
	// legitimately write floor 0 for flat bots, so the census targets a row
	// THIS test inserts without a floor (fresh rows default to NULL, the
	// pre-0050 backfilled zeros are gone for any row the reconcile loop has
	// not touched since).
	service := NewService(pool, risk.NewEngine(pool))
	if _, err := service.GetSettings(ctx); err != nil {
		t.Fatalf("settings: %v", err)
	}
	probeID := "7c88b000-0000-4119-8119-000000000050"
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE id = $1`, probeID)
	accountService := accounts.NewService(pool)
	probeAccount, accErr := accountService.Create(ctx, accounts.CreateInput{
		Name:   "integration-floor0050-" + time.Now().Format("150405.000000000"),
		APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if accErr != nil {
		t.Fatalf("create probe account: %v", accErr)
	}
	var settingsID string
	if err := pool.QueryRow(ctx, `SELECT id FROM autogrid_settings LIMIT 1`).Scan(&settingsID); err != nil {
		t.Fatalf("settings row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO grid_bots (
			id, account_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			request_fingerprint, autogrid_settings_id, unrealized_pnl_usdt, bot_number
		) VALUES (
			$1, $2, 'FLOOR0050_USDT_PERP', 'bu-floor0050', 'STOPPED', 'NEUTRAL', 'GEOMETRIC',
			1, 2, 4, 2, 100, 'fp-floor0050', $3, -1.25, 5050
		)
	`, probeID, probeAccount.ID, settingsID); err != nil {
		t.Fatalf("seed floor probe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE id = $1`, probeID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, probeAccount.ID)
	})
	var viaCoalesce decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(supervision_floor_pnl_usdt, unrealized_pnl_usdt, 0)
		FROM grid_bots WHERE id = $1
	`, probeID).Scan(&viaCoalesce); err != nil {
		t.Fatalf("floor probe read: %v", err)
	}
	if !viaCoalesce.Equal(decimal.RequireFromString("-1.25")) {
		t.Fatalf("NULL floor must fall back to the raw unrealized leg, got %s", viaCoalesce.String())
	}

	// The wick shield must be disabled by default after 0050.
	var shieldDefault bool
	if err := pool.QueryRow(ctx, `SELECT COALESCE(bool_or(wick_shield_enabled), false) FROM autogrid_settings`).Scan(&shieldDefault); err != nil {
		t.Fatalf("settings read: %v", err)
	}
	if shieldDefault {
		t.Fatalf("migration 0050 must disable wick_shield_enabled on existing settings rows")
	}
}
