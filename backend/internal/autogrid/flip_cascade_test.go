package autogrid

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// ── package E (v2.0.140): the first direction-flip experiment ──────────────
//
// TestDgtDirectionFlip_Integration drives the full flip lane against the
// disposable DB + mock exchange:
//
//  1. a NEUTRAL REAL bot underwater under CONFIRMED_DUMP takes the emergency
//     exit (STOP_REQUESTED + EMERGENCY_OFI_DUMP + queued DGT intent);
//  2. at settle time the dump is STILL confirmed and a >$50M/1h
//     long-liquidation cascade runs — the intent must FLIP: outcome
//     "flip_short", a DIRECTION_FLIP event on the closed bot, zero exchange
//     creates, zero new grid rows, and an out-of-turn cascade-short scan
//     queued for the scanner.
func TestDgtDirectionFlip_Integration(t *testing.T) {
	ctx := context.Background()
	h := newRealDeployHarness(t, 10, "BTC_USDT_PERP")
	pool := h.pool
	h.cleanupSymbol(t, "BTC_USDT_PERP")
	settings := *h.settings
	settings.StopForecastMode = "ACTIVE"
	settings.DgtRedeployEnabled = true
	// maybeQueueCascadeShortScan only arms on a RUNNING autopilot.
	settings.Status = "RUNNING"
	clearCascadeWindow(t, pool)

	const botNumber = 970
	botID := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, botNumber)
	runEmergencyExitUnderDump(t, pool, h.worker, settings, botID, botNumber, "BTC_USDT_PERP")

	var gridRowsBefore int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE account_id = $1
	`, h.account.ID).Scan(&gridRowsBefore); err != nil {
		t.Fatalf("count grid rows: %v", err)
	}

	// The dump is still confirmed at settle time, and the cascade is live:
	// $75M of forced long unwinding inside the detector's 1h window.
	seedConfirmedDump(t, h.worker, "BTC_USDT_PERP")
	seedLongLiquidationCascade(t, pool, "BTC_USDT_PERP")
	settleRealBot(t, pool, botID)

	h.worker.processDgtRealRedeployIntents(ctx, settings)

	outcome := readDgtOutcome(t, pool, botID)
	if outcome == nil {
		t.Fatal("expected dgtRedeployOutcome after intent processing")
	}
	if *outcome != "flip_short" {
		t.Fatalf("expected outcome flip_short, got %q", *outcome)
	}
	var flipEvents int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DIRECTION_FLIP'
	`, botID).Scan(&flipEvents); err != nil || flipEvents != 1 {
		t.Fatalf("expected exactly one DIRECTION_FLIP event, got %d (err %v)", flipEvents, err)
	}
	var flipTo string
	if err := pool.QueryRow(ctx, `
		SELECT details->>'to' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DIRECTION_FLIP'
	`, botID).Scan(&flipTo); err != nil || flipTo != "SHORT" {
		t.Fatalf("expected DIRECTION_FLIP details.to = SHORT, got %q (err %v)", flipTo, err)
	}
	if calls := h.mock.createCalls.Load(); calls != 0 {
		t.Fatalf("flip must not reach the exchange create, got %d calls", calls)
	}
	var gridRowsAfter int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE account_id = $1
	`, h.account.ID).Scan(&gridRowsAfter); err != nil {
		t.Fatalf("count grid rows after: %v", err)
	}
	if gridRowsAfter != gridRowsBefore {
		t.Fatalf("flip must not create a grid row: before %d, after %d", gridRowsBefore, gridRowsAfter)
	}
	var cascadeScans int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM control_commands
		WHERE command_type = 'autogrid.scan'
		  AND arguments->>'cascadeShort' = 'true'
	`).Scan(&cascadeScans); err != nil || cascadeScans == 0 {
		t.Fatalf("expected the out-of-turn cascade-short scan to be queued, got %d (err %v)", cascadeScans, err)
	}
}

// TestDgtDirectionFlipRequiresCascade_Integration is the no-cascade control:
// identical setup EXCEPT no liquidation rows — the cascade leg of the flip
// conjunction is false, so the normal same-direction path runs and is blocked
// by the v2.0.136 OFI re-entry veto (the dump still being confirmed is
// precisely the knife CanEnter refuses to reload).
func TestDgtDirectionFlipRequiresCascade_Integration(t *testing.T) {
	ctx := context.Background()
	h := newRealDeployHarness(t, 10, "BTC_USDT_PERP")
	pool := h.pool
	h.cleanupSymbol(t, "BTC_USDT_PERP")
	settings := *h.settings
	settings.StopForecastMode = "ACTIVE"
	settings.DgtRedeployEnabled = true
	settings.Status = "RUNNING"
	clearCascadeWindow(t, pool)

	const botNumber = 971
	botID := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, botNumber)
	runEmergencyExitUnderDump(t, pool, h.worker, settings, botID, botNumber, "BTC_USDT_PERP")

	seedConfirmedDump(t, h.worker, "BTC_USDT_PERP")
	settleRealBot(t, pool, botID)

	h.worker.processDgtRealRedeployIntents(ctx, settings)

	outcome := readDgtOutcome(t, pool, botID)
	if outcome == nil {
		t.Fatal("expected dgtRedeployOutcome after intent processing")
	}
	if *outcome == "flip_short" {
		t.Fatal("flip must NOT fire without an active liquidation cascade")
	}
	var skipReason string
	err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DGT_REDEPLOY_SKIPPED'
		ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&skipReason)
	if err != nil {
		t.Fatalf("DGT_REDEPLOY_SKIPPED event missing: %v", err)
	}
	if !strings.Contains(skipReason, "OFI re-entry veto") {
		t.Fatalf("expected the OFI re-entry veto skip reason, got %q", skipReason)
	}
	var flipEvents int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DIRECTION_FLIP'
	`, botID).Scan(&flipEvents); err != nil || flipEvents != 0 {
		t.Fatalf("no DIRECTION_FLIP event may exist without a cascade, got %d (err %v)", flipEvents, err)
	}
	var cascadeScans int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM control_commands
		WHERE command_type = 'autogrid.scan'
		  AND arguments->>'cascadeShort' = 'true'
	`).Scan(&cascadeScans); err != nil || cascadeScans != 0 {
		t.Fatalf("cascade-short scan must not be queued without a cascade, got %d (err %v)", cascadeScans, err)
	}
	if calls := h.mock.createCalls.Load(); calls != 0 {
		t.Fatalf("vetoed re-deploy must not reach the exchange, got %d calls", calls)
	}
}

// ── package F (v2.0.140): the chain-loss budget ────────────────────────────
//
// TestDgtChainLossBudget_Integration verifies the F1 gate: a symbol whose
// break-family closes already bled more than 2× the slot's designed stop
// ceiling inside 24h stays with the scanner even when every other DGT gate
// would pass (flow cleared to a fresh NEUTRAL, cascade inactive, ladder far
// from its cap).
func TestDgtChainLossBudget_Integration(t *testing.T) {
	ctx := context.Background()
	h := newRealDeployHarness(t, 10, "BTC_USDT_PERP")
	pool := h.pool
	h.cleanupSymbol(t, "BTC_USDT_PERP")
	settings := *h.settings
	settings.StopForecastMode = "ACTIVE"
	settings.DgtRedeployEnabled = true
	clearCascadeWindow(t, pool)

	// A prior break-family close that bled past the chain budget: 2× the
	// slot's designed stop ceiling + $1. insertEmergencyRealBot commits a
	// $250 slot, which the fresh intent resolves as its slot budget — the
	// same budget the gate derives the chain ceiling from.
	const priorNumber = 972
	priorID := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, priorNumber)
	chainBudget := chainBudgetFor(settings)
	priorLoss := chainBudget.Add(d("1"))
	if _, err := pool.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOPPED', closed_reason = 'RANGE_BREAK_DOWN',
		    realized_pnl_usdt = $2,
		    closed_at = NOW() - INTERVAL '1 hour', updated_at = NOW() - INTERVAL '1 hour'
		WHERE id = $1
	`, priorID, priorLoss.Neg()); err != nil {
		t.Fatalf("seed prior chain loss: %v", err)
	}

	// The fresh intent: emergency exit under a confirmed dump, then the flow
	// clears so the microstructure re-entry gates PASS and the chain budget
	// is the deciding blocker.
	const botNumber = 973
	botID := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, botNumber)
	runEmergencyExitUnderDump(t, pool, h.worker, settings, botID, botNumber, "BTC_USDT_PERP")
	seedNeutralMicrostructure(t, h.worker, "BTC_USDT_PERP")
	settleRealBot(t, pool, botID)

	h.worker.processDgtRealRedeployIntents(ctx, settings)

	outcome := readDgtOutcome(t, pool, botID)
	if outcome == nil {
		t.Fatal("expected dgtRedeployOutcome after intent processing")
	}
	if *outcome == "flip_short" {
		t.Fatal("flow-cleared NEUTRAL must take the normal path, not the flip")
	}
	if *outcome != "blocked" {
		t.Fatalf("expected the chain-budgeted re-deploy to be blocked, got %q", *outcome)
	}
	var skipReason string
	err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DGT_REDEPLOY_SKIPPED'
		ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&skipReason)
	if err != nil {
		t.Fatalf("DGT_REDEPLOY_SKIPPED event missing: %v", err)
	}
	if !strings.Contains(skipReason, "цепочка прорывов") {
		t.Fatalf("expected the chain-loss skip reason, got %q", skipReason)
	}
	if calls := h.mock.createCalls.Load(); calls != 0 {
		t.Fatalf("chain-budgeted re-deploy must not reach the exchange, got %d calls", calls)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// runEmergencyExitUnderDump drives radarMicrostructureEmergencyExit the exact
// way the radar would for a NEUTRAL REAL bot underwater under a confirmed
// dump, then asserts the protective close and the queued DGT intent exist.
func runEmergencyExitUnderDump(t *testing.T, pool *pgxpool.Pool, worker *Worker, settings Settings, botID string, botNumber int, symbol string) {
	t.Helper()
	b := radarInput{
		botID: botID, botNumber: botNumber, botSource: "REAL",
		symbol: symbol, direction: "NEUTRAL",
		inventorySide: 1, // long inventory during confirmed dump
		ofiRegime:     "CONFIRMED_DUMP",
		ofiActionable: true,
		price:         d("96.0"),
		lower:         d("95.0"),
		upper:         d("105.0"),
		total:         d("-0.50"), // below the old -$1.00 DGT threshold
	}
	if !worker.radarMicrostructureEmergencyExit(context.Background(), settings, b) {
		t.Fatalf("bot %d: expected emergency exit to execute", botNumber)
	}
	var status, closedReason string
	var pending *string
	if err := pool.QueryRow(context.Background(), `
		SELECT status, COALESCE(closed_reason, ''), model_state->>'dgtRedeployPendingAt'
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&status, &closedReason, &pending); err != nil {
		t.Fatalf("bot %d: query: %v", botNumber, err)
	}
	if status != "STOP_REQUESTED" || closedReason != "EMERGENCY_OFI_DUMP" {
		t.Fatalf("bot %d: expected STOP_REQUESTED/EMERGENCY_OFI_DUMP, got %s/%s",
			botNumber, status, closedReason)
	}
	if pending == nil {
		t.Fatalf("bot %d: expected the queued DGT re-deploy intent (dgtRedeployPendingAt)", botNumber)
	}
}

// settleRealBot moves a STOP_REQUESTED row to the terminal settle the intent
// pickup requires (the closed-bot sync's job in production).
func settleRealBot(t *testing.T, pool *pgxpool.Pool, botID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE grid_bots
		SET status = 'STOPPED', closed_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("settle %s: %v", botID, err)
	}
}

// clearCascadeWindow gives every cascade-sensitive assertion a cold, dete-
// rministic start: the detector sums the trailing hour fleet-wide, so stale
// rows (from a previous run or a sibling test) would flip the conjunction.
// The cleanup removes exactly what this test family seeds — liquidation rows
// inside the 2h lookback, cascade-short scan commands (bucketed idempotency
// keys would otherwise collide on a re-run inside 15 minutes), and any
// autogrid.scan command that the "scan recently" fence would trip on.
func clearCascadeWindow(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	wipe := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM liquidation_events WHERE captured_at > NOW() - INTERVAL '2 hours'`)
		_, _ = pool.Exec(ctx, `DELETE FROM control_commands WHERE command_type = 'autogrid.scan'`)
	}
	wipe()
	t.Cleanup(wipe)
}

// seedLongLiquidationCascade crafts the rows CheckLiquidationCascade sums:
// forced LONG unwinding worth $75M inside the trailing hour (three $25M
// prints, the freshest 5 minutes old) — above the $50M threshold the flip
// lane and maybeQueueCascadeShortScan share.
func seedLongLiquidationCascade(t *testing.T, pool *pgxpool.Pool, symbol string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		capturedAt := time.Now().Add(-time.Duration(5+i*5) * time.Minute)
		if _, err := pool.Exec(ctx, `
			INSERT INTO liquidation_events (symbol, side, value_usd, captured_at)
			VALUES ($1, 'long', $2, $3)
		`, symbol, 25_000_000, capturedAt); err != nil {
			t.Fatalf("seed cascade row %d: %v", i, err)
		}
	}
	var usd float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(value_usd), 0)::float8 FROM liquidation_events
		WHERE captured_at > NOW() - INTERVAL '1 hour' AND side = 'long'
	`).Scan(&usd); err != nil || usd <= 50_000_000 {
		t.Fatalf("cascade seed must sum > $50M for the detector, got %.0f (err %v)", usd, err)
	}
}

// chainBudgetFor is the test-side twin of the F1 threshold: 2× the slot's
// designed stop ceiling for the harness's $250 slot (kept next to the tests
// so a cap-formula change fails here, not silently in production).
func chainBudgetFor(settings Settings) decimal.Decimal {
	return tranche2MaxLossCap(d("250"), settings.Leverage).Mul(d("2"))
}
