package autogrid

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
)

// TestRadarMicrostructureEmergencyExit_NeutralDgtDecoupled_Integration verifies
// the full protective-exit → DGT re-entry chain against a disposable DB and the
// mock exchange harness:
//
//  1. NEUTRAL REAL bot with total = -$0.50 triggers the emergency exit
//     (STOP_REQUESTED) and queues the DGT re-deploy intent in model_state.
//  2. While the dump is STILL CONFIRMED (engine seeded), the terminal-settle
//     intent pickup must BLOCK the re-deploy on the v2.0.136 OFI re-entry
//     gate — outcome "blocked", a DGT_REDEPLOY_SKIPPED event carrying the OFI
//     reason, and zero exchange create calls.
//  3. Once the flow clears (engine reset), a second intent must DEPLOY: the
//     mock exchange create is hit exactly once and a new native grid row with
//     the returned buOrderId exists — outcome "deployed".
func TestRadarMicrostructureEmergencyExit_NeutralDgtDecoupled_Integration(t *testing.T) {
	ctx := context.Background()
	h := newRealDeployHarness(t, 10, "BTC_USDT_PERP")
	pool := h.pool
	settings := *h.settings
	settings.StopForecastMode = "ACTIVE"
	settings.DgtRedeployEnabled = true

	bot1 := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, 990)
	bot2 := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, 991)
	bot3 := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, 993)

	runEmergencyExit := func(botID string, botNumber int) {
		t.Helper()
		b := radarInput{
			botID: botID, botNumber: botNumber, botSource: "REAL",
			symbol: "BTC_USDT_PERP", direction: "NEUTRAL",
			inventorySide: 1, // long inventory during confirmed dump
			ofiRegime:     "CONFIRMED_DUMP",
			ofiActionable: true,
			price:         d("96.0"),
			lower:         d("95.0"),
			upper:         d("105.0"),
			total:         d("-0.50"), // below the old -$1.00 DGT threshold
		}
		if !h.worker.radarMicrostructureEmergencyExit(ctx, settings, b) {
			t.Fatalf("bot %d: expected emergency exit to execute", botNumber)
		}
		var status, closedReason string
		if err := pool.QueryRow(ctx, `
			SELECT status, COALESCE(closed_reason, '') FROM grid_bots WHERE id = $1
		`, botID).Scan(&status, &closedReason); err != nil {
			t.Fatalf("bot %d: query: %v", botNumber, err)
		}
		if status != "STOP_REQUESTED" || closedReason != "EMERGENCY_OFI_DUMP" {
			t.Fatalf("bot %d: expected STOP_REQUESTED/EMERGENCY_OFI_DUMP, got %s/%s",
				botNumber, status, closedReason)
		}
	}
	runEmergencyExit(bot1, 990)
	runEmergencyExit(bot2, 991)
	runEmergencyExit(bot3, 993)

	// ── Part 1: the dump is still confirmed → re-entry must be blocked ──
	seedConfirmedDump(t, h.worker, "BTC_USDT_PERP")

	settleBot := func(botID string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'STOPPED', closed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, botID); err != nil {
			t.Fatalf("settle %s: %v", botID, err)
		}
	}
	settleBot(bot1)

	h.worker.processDgtRealRedeployIntents(ctx, settings)

	outcome1 := readDgtOutcome(t, pool, bot1)
	if outcome1 == nil {
		t.Fatal("bot1: expected dgtRedeployOutcome after intent processing")
	}
	if *outcome1 != "blocked" {
		t.Fatalf("bot1: expected outcome blocked under continuing CONFIRMED_DUMP, got %q", *outcome1)
	}
	var skipReason string
	err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DGT_REDEPLOY_SKIPPED'
		ORDER BY created_at DESC LIMIT 1
	`, bot1).Scan(&skipReason)
	if err != nil {
		t.Fatalf("bot1: DGT_REDEPLOY_SKIPPED event missing: %v", err)
	}
	if !strings.Contains(skipReason, "OFI re-entry veto") {
		t.Fatalf("bot1: expected OFI re-entry veto skip reason, got %q", skipReason)
	}
	if calls := h.mock.createCalls.Load(); calls != 0 {
		t.Fatalf("bot1: blocked re-deploy must not reach the exchange create, got %d calls", calls)
	}

	// ── Part 1.5: feed went STALE → an EMERGENCY_OFI_* re-entry must stay
	// blocked: missing data is not proof the danger is gone (v2.0.137) ──
	seedStaleBook(t, h.worker, "BTC_USDT_PERP")
	settleBot(bot3)

	h.worker.processDgtRealRedeployIntents(ctx, settings)

	outcome3 := readDgtOutcome(t, pool, bot3)
	if outcome3 == nil {
		t.Fatal("bot3: expected dgtRedeployOutcome after intent processing")
	}
	if *outcome3 != "blocked" {
		t.Fatalf("bot3: expected outcome blocked under a STALE feed, got %q", *outcome3)
	}
	var staleReason string
	if err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'DGT_REDEPLOY_SKIPPED'
		ORDER BY created_at DESC LIMIT 1
	`, bot3).Scan(&staleReason); err != nil {
		t.Fatalf("bot3: DGT_REDEPLOY_SKIPPED event missing: %v", err)
	}
	if !strings.Contains(staleReason, "нормализации") {
		t.Fatalf("bot3: expected the no-fresh-confirmation skip reason, got %q", staleReason)
	}
	if calls := h.mock.createCalls.Load(); calls != 0 {
		t.Fatalf("bot3: stale-blocked re-deploy must not reach the exchange, got %d calls", calls)
	}
	// Simulate the hour passing: the consumed emergency closes (bot1, bot3)
	// age out of the 1h protective-close circuit breaker so the breaker does
	// not mask the Part-2 deploy-path assertions.
	if _, err := pool.Exec(ctx, `
		UPDATE grid_bots
		SET closed_at = NOW() - INTERVAL '2 hours'
		WHERE id IN ($1, $2)
	`, bot1, bot3); err != nil {
		t.Fatalf("age out processed closes: %v", err)
	}

	// ── Part 2: the flow cleared → the second intent must deploy ──
	seedNeutralMicrostructure(t, h.worker, "BTC_USDT_PERP")
	// NEUTRAL re-entry consults the liquidation-source health gate; an empty
	// feed-health table reads as "unstable" and would mask the deploy path.
	if _, err := pool.Exec(ctx, `
		INSERT INTO liquidation_feed_health (source, connected, last_message_at)
		VALUES ('bybit', true, NOW())
		ON CONFLICT (source) DO UPDATE SET connected = true, last_message_at = NOW()
	`); err != nil {
		t.Fatalf("seed liquidation feed health: %v", err)
	}
	settleBot(bot2)

	h.worker.processDgtRealRedeployIntents(ctx, settings)

	outcome2 := readDgtOutcome(t, pool, bot2)
	if outcome2 == nil {
		t.Fatal("bot2: expected dgtRedeployOutcome after intent processing")
	}
	if *outcome2 != "deployed" {
		var why string
		_ = pool.QueryRow(ctx, `
			SELECT details->>'reason' FROM bot_execution_events
			WHERE bot_id = $1 AND event_type = 'DGT_REDEPLOY_SKIPPED'
			ORDER BY created_at DESC LIMIT 1
		`, bot2).Scan(&why)
		t.Fatalf("bot2: expected outcome deployed after flow cleared, got %q (skip reason: %q)", *outcome2, why)
	}
	if calls := h.mock.createCalls.Load(); calls != 1 {
		t.Fatalf("bot2: expected exactly one exchange create, got %d", calls)
	}
	var newBots int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE account_id = $1 AND bu_order_id = 'REALSKIP-1'
	`, h.account.ID).Scan(&newBots); err != nil || newBots != 1 {
		t.Fatalf("bot2: expected the re-deployed native grid row (buOrderId REALSKIP-1), count=%d err=%v", newBots, err)
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

	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: fmt.Sprintf("itest-em-retry-%d", time.Now().UnixNano()),
		APIKey: "itest-key", APISecret: "itest-secret",
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
	botID := insertEmergencyRealBotStatus(t, pool, account.ID, settings.ID, 998, "STOPPED")

	b := radarInput{
		botID: botID, botNumber: 998, botSource: "REAL",
		symbol: "BTC_USDT_PERP", direction: "LONG",
		inventorySide: 1,
		ofiRegime:     "CONFIRMED_DUMP",
		ofiActionable: true,
		price:         d("96.0"),
		lower:         d("95.0"),
		upper:         d("105.0"),
		total:         d("-10.0"),
	}

	if worker.radarMicrostructureEmergencyExit(ctx, *settings, b) {
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

// TestRadarBreakFlip_OFIEscapeLane_Integration pins the v2.0.136 uuid-cast
// fix in radarBreakFlip: with a confirmed-adverse flow and edge progress past
// the accelerated threshold, the escape lane must actually take over —
// STOP_REQUESTED with a RANGE_BREAK_* reason and a queued REAL DGT intent.
// (The pre-fix COALESCE(account_id,'') failed at plan time on every call, so
// this lane — and its DGT intent — never fired in production.)
func TestRadarBreakFlip_OFIEscapeLane_Integration(t *testing.T) {
	ctx := context.Background()
	h := newRealDeployHarness(t, 10, "BTC_USDT_PERP")
	pool := h.pool
	settings := *h.settings
	settings.DgtRedeployEnabled = true

	botID := insertEmergencyRealBot(t, pool, h.account.ID, settings.ID, 992)

	b := radarInput{
		botID: botID, botNumber: 992, botSource: "REAL",
		symbol: "BTC_USDT_PERP", direction: "NEUTRAL",
		inventorySide: 1, // long inventory fleeing a confirmed dump
		ofiRegime:     "CONFIRMED_DUMP",
		ofiActionable: true,
		price:         d("96.0"),
		lower:         d("95.0"),
		upper:         d("105.0"),
		total:         d("-2.00"), // below breakFlipMinFloat (-$1.00)
	}
	// OFI accelerated trigger: progress (105−96)/10 = 0.90 ≥ 0.60 — the
	// velocity/band preconditions of the standard trigger stay unmet.
	if !h.worker.radarBreakFlip(ctx, settings, b, radarScores{}, 0) {
		t.Fatal("expected the OFI-accelerated escape lane to take over")
	}

	var status, closedReason string
	var pending *string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason, ''), model_state->>'dgtRedeployPendingAt'
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&status, &closedReason, &pending); err != nil {
		t.Fatalf("query bot: %v", err)
	}
	if status != "STOP_REQUESTED" || closedReason != "RANGE_BREAK_DOWN" {
		t.Fatalf("expected STOP_REQUESTED/RANGE_BREAK_DOWN, got %s/%s", status, closedReason)
	}
	if pending == nil {
		t.Fatal("expected the break-flip DGT re-deploy intent to be queued (dgtRedeployPendingAt)")
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// insertEmergencyRealBot seeds one RUNNING REAL grid row with the exact
// column set the grid_bots migrations define (no paper-only columns).
func insertEmergencyRealBot(t *testing.T, pool *pgxpool.Pool, accountID, settingsID string, botNumber int) string {
	t.Helper()
	return insertEmergencyRealBotStatus(t, pool, accountID, settingsID, botNumber, "RUNNING")
}

func insertEmergencyRealBotStatus(t *testing.T, pool *pgxpool.Pool, accountID, settingsID string, botNumber int, status string) string {
	t.Helper()
	ctx := context.Background()
	var botID string
	buOrderID := fmt.Sprintf("EM-%d-%d", botNumber, time.Now().UnixNano())
	fp := fmt.Sprintf("fp-em-%d-%d", botNumber, time.Now().UnixNano())
	err := pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			extra_margin, request_fingerprint, execution_mode, reconciliation_state,
			realized_pnl_usdt, unrealized_pnl_usdt,
			bot_number, created_at, updated_at
		) VALUES (
			$1, $2, 'BTC_USDT_PERP', $3, $4, 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 250,
			0, $5, 'REAL', 'REST_AUTHORITATIVE_OK',
			0, -0.50,
			$6, NOW(), NOW()
		)
		RETURNING id
	`, accountID, settingsID, buOrderID, status, fp, botNumber).Scan(&botID)
	if err != nil {
		t.Fatalf("insert real bot %d: %v", botNumber, err)
	}
	return botID
}

// seedConfirmedDump drives the worker's live OFI engine into an actionable
// CONFIRMED_DUMP regime on the symbol: three 5s windows of ask-heavy books,
// ≥$5k taker sells each, and >0.15% downward displacement (triple confluence
// under DefaultOFIEngineConfig).
func seedConfirmedDump(t *testing.T, worker *Worker, symbol string) {
	t.Helper()
	now := time.Now()
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i) * 5 * time.Second)
		price := d("96.00").Sub(d("0.10").Mul(decimal.NewFromInt(int64(i))))
		worker.ofiEngine.IngestL2(symbol,
			[]pionex.DepthLevel{{Price: price.Sub(d("0.02")), Amount: d("50.0")}},
			[]pionex.DepthLevel{{Price: price, Amount: d("500.0")}},
			ts,
		)
		worker.ofiEngine.IngestTrade(symbol, pionex.Trade{
			Symbol: symbol, Side: "SELL",
			Price: price.Sub(d("0.02")), Size: d("100.0"),
			Time: ts.UnixMilli(),
		})
		worker.ofiEngine.FinalizeWindow(symbol, ts.Add(5*time.Second))
	}
	analysis := worker.ofiEngine.Analyze(symbol)
	if analysis.Regime != "CONFIRMED_DUMP" || !analysis.IsActionable() {
		t.Fatalf("seed: expected actionable CONFIRMED_DUMP, got %s (fresh=%v synced=%v reason=%s)",
			analysis.Regime, analysis.IsFresh, analysis.IsSynced, analysis.Reason)
	}
}

// seedStaleBook stamps the symbol's book with an update older than
// MaxStaleness (15s) so Analyze reports the STALE regime — the "feed died,
// danger unproven" state an emergency re-entry must fail closed on.
func seedStaleBook(t *testing.T, worker *Worker, symbol string) {
	t.Helper()
	worker.ofiEngine.IngestL2(symbol,
		[]pionex.DepthLevel{{Price: d("95.98"), Amount: d("300.0")}},
		[]pionex.DepthLevel{{Price: d("96.00"), Amount: d("300.0")}},
		time.Now().Add(-60*time.Second),
	)
	analysis := worker.ofiEngine.Analyze(symbol)
	if analysis.Regime != "STALE" {
		t.Fatalf("seed: expected STALE book, got %s (fresh=%v synced=%v reason=%s)",
			analysis.Regime, analysis.IsFresh, analysis.IsSynced, analysis.Reason)
	}
}

// seedNeutralMicrostructure rolls fresh balanced windows so the regime decays
// back to a fresh NEUTRAL — the realistic "flow cleared" state a later DGT
// intent re-check sees (ResetSymbol would leave DESYNC, which CanEnter
// deliberately fails closed on).
func seedNeutralMicrostructure(t *testing.T, worker *Worker, symbol string) {
	t.Helper()
	now := time.Now()
	for i := 0; i < 4; i++ {
		ts := now.Add(time.Duration(i) * 5 * time.Second)
		worker.ofiEngine.IngestL2(symbol,
			[]pionex.DepthLevel{{Price: d("95.98"), Amount: d("300.0")}},
			[]pionex.DepthLevel{{Price: d("96.00"), Amount: d("300.0")}},
			ts,
		)
		worker.ofiEngine.IngestTrade(symbol, pionex.Trade{
			Symbol: symbol, Side: "BUY",
			Price: d("96.00"), Size: d("50.0"), Time: ts.UnixMilli(),
		})
		worker.ofiEngine.IngestTrade(symbol, pionex.Trade{
			Symbol: symbol, Side: "SELL",
			Price: d("95.98"), Size: d("50.0"), Time: ts.Add(time.Second).UnixMilli(),
		})
		worker.ofiEngine.FinalizeWindow(symbol, ts.Add(5*time.Second))
	}
	analysis := worker.ofiEngine.Analyze(symbol)
	if analysis.Regime != "NEUTRAL" {
		t.Fatalf("seed: expected NEUTRAL after balanced windows, got %s (fresh=%v synced=%v reason=%s)",
			analysis.Regime, analysis.IsFresh, analysis.IsSynced, analysis.Reason)
	}
}

func readDgtOutcome(t *testing.T, pool *pgxpool.Pool, botID string) *string {
	t.Helper()
	var outcome *string
	if err := pool.QueryRow(context.Background(), `
		SELECT model_state->>'dgtRedeployOutcome' FROM grid_bots WHERE id = $1
	`, botID).Scan(&outcome); err != nil {
		t.Fatalf("read outcome %s: %v", botID, err)
	}
	return outcome
}
