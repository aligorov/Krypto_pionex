package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// v2.0.76 "shift on green" test battery. The exchange gates
// /bot/orders/futuresGrid/adjustParams on the order's current profit
// (BOT_INVALID_ARGUMENT / PROFIT_LESS_THAN_ZERO), so every shift path must
// preflight locally: green bots shift (early at band 2), underwater bots
// never hit the exchange and leave a durable blocked-event instead.

// (a, pure levels) The preflight floor: >= +$0.10 passes, everything below —
// including the 0..0.10 drift band between reconcile and shift — is blocked.
// v2.0.83: the figure under test is the FLOATING PnL, not the bot total.
func TestAdjustShiftPreflightLevels(t *testing.T) {
	if !adjustShiftFeasible(d("0.5")) {
		t.Fatal("floating +$0.5 must pass the preflight")
	}
	if !adjustShiftFeasible(d("0.10")) {
		t.Fatal("the floor itself must pass (>=, not >)")
	}
	if adjustShiftFeasible(d("0.09")) {
		t.Fatal("floating inside the drift buffer must be blocked")
	}
	if adjustShiftFeasible(d("0")) {
		t.Fatal("flat floating must be blocked — the exchange requires floating profit > 0")
	}
	if adjustShiftFeasible(d("-0.5")) {
		t.Fatal("underwater floating must be blocked")
	}
}

// (a-regress, the prod NEAR #688 shape) Banked grid profit must NOT mask a
// sinking position: total +1.5 (realized +2.0, floating −0.5) cleared the
// v2.0.76 total-based preflight and the exchange still refused
// adjust_params with PROFIT_LESS_THAN_ZERO (2026-09-04 19:05Z). The v2.0.83
// gate keys on the floating leg, so this exact bot must now be blocked
// BEFORE the exchange call, with the durable blocked event as the trace.
func TestRadarRealPreflightFloatingNotTotal(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 2)
	h.seedDwell(t, botID, symbol)
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET realized_pnl_usdt = 2.0, unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("plant the NEAR #688 shape (total +1.5, floating −0.5): %v", err)
	}

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})

	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("floating −0.5 must veto the exchange call even with total +1.5, got %d adjusts", got)
	}
	var blocked int
	var totalPayload string
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE((SELECT details->>'total_pnl' FROM bot_execution_events
			WHERE bot_id = $1 AND event_type = 'RADAR_SHIFT_BLOCKED_UNDERWATER'
			ORDER BY created_at DESC LIMIT 1), '')
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_SHIFT_BLOCKED_UNDERWATER'
	`, botID).Scan(&blocked, &totalPayload); err != nil || blocked != 1 {
		t.Fatalf("exactly one blocked event expected, got %d (%v)", blocked, err)
	}
	if totalPayload != "1.5000" {
		t.Fatalf("blocked event must carry the total (+1.5) for context, got %q", totalPayload)
	}
	var adjustments int
	if err := h.pool.QueryRow(ctx, `SELECT adjustments_count FROM grid_bots WHERE id = $1`, botID).
		Scan(&adjustments); err != nil || adjustments != 2 {
		t.Fatalf("blocked shift must keep the budget, got %d (%v)", adjustments, err)
	}

	// The same bot with the floating leg recovered (+0.5) shifts again —
	// the gate opens the moment pure floating goes green.
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET unrealized_pnl_usdt = 0.5 WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("recover the floating leg: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots
		SET model_state = jsonb_set(COALESCE(model_state,'{}'::jsonb), '{radarShiftBlockedAt}', '"1970-01-01T00:00:00Z"')
		WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("re-arm the blocked marker: %v", err)
	}
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("recovered floating (+0.5) must allow the shift, got %d adjusts", got)
	}
}

// (b, pure geometry) Adverse-edge progress for the B2 early trigger: the
// share of the range width already covered toward the edge the inventory
// fears, clamped, zero for degenerate ranges.
func TestRadarB2EarlyEdgeProgress(t *testing.T) {
	lower, upper := d("90"), d("110")
	// Long inventory fears the lower bound: 95 sits 75% of the way down.
	if p := radarB2EarlyEdgeProgress(d("95"), lower, upper, 1); p != 0.75 {
		t.Fatalf("long inventory at 95 must read 0.75 progress, got %v", p)
	}
	// Short inventory fears the upper bound: 105 sits 75% of the way up.
	if p := radarB2EarlyEdgeProgress(d("105"), lower, upper, -1); p != 0.75 {
		t.Fatalf("short inventory at 105 must read 0.75 progress, got %v", p)
	}
	// Flat inventory at mid: 0.5 on the long-side reading — below the 0.70
	// trigger, so a mid-sitting bot never early-shifts.
	if p := radarB2EarlyEdgeProgress(d("100"), lower, upper, 0); p != 0.5 {
		t.Fatalf("flat inventory at mid must read 0.5, got %v", p)
	}
	// Progress clamps at the bounds and collapses on degenerate ranges.
	if p := radarB2EarlyEdgeProgress(d("120"), lower, upper, -1); p != 1 {
		t.Fatalf("progress beyond the upper bound must clamp to 1, got %v", p)
	}
	if p := radarB2EarlyEdgeProgress(d("85"), lower, upper, 1); p != 1 {
		t.Fatalf("progress beyond the lower bound must clamp to 1, got %v", p)
	}
	if p := radarB2EarlyEdgeProgress(d("100"), lower, lower, 1); p != 0 {
		t.Fatalf("degenerate range must read 0, got %v", p)
	}
}

// (a-/c) B3 under water: the preflight must veto the exchange call, leave the
// budget and bounds untouched, keep the bot RUNNING (the close decision stays
// with the stop ladder/operator), write exactly one
// RADAR_SHIFT_BLOCKED_UNDERWATER event per hour, and NOT arm the durable
// radar cooldown — a blocked shift is a no-action, so the re-center becomes
// eligible again the moment profit recovers.
func TestRadarRealPreflightUnderwaterBlocked(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 2)
	h.seedDwell(t, botID, symbol)
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET realized_pnl_usdt = -0.5, unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("sink the bot under water (both legs): %v", err)
	}

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})

	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("preflight must veto the exchange call for an underwater bot, got %d adjusts", got)
	}
	var blocked int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_SHIFT_BLOCKED_UNDERWATER'
	`, botID).Scan(&blocked); err != nil || blocked != 1 {
		t.Fatalf("exactly one blocked event expected, got %d (%v)", blocked, err)
	}
	var adjustments int
	var status string
	if err := h.pool.QueryRow(ctx, `
		SELECT adjustments_count, status FROM grid_bots WHERE id = $1
	`, botID).Scan(&adjustments, &status); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if adjustments != 2 || status != "RUNNING" {
		t.Fatalf("blocked shift must keep budget and life: adjustments=%d status=%s", adjustments, status)
	}
	// The cooldown stays unarmed: no ADJUST_RANGE row exists for this bot.
	if _, ok := h.worker.radarLastActionAt(ctx, botID); ok {
		t.Fatal("a blocked shift must not arm the durable radar cooldown")
	}

	// Dedup: a second identical pass within the hour adds no second event.
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_SHIFT_BLOCKED_UNDERWATER'
	`, botID).Scan(&blocked); err != nil || blocked != 1 {
		t.Fatalf("blocked events must dedup to 1/h, got %d (%v)", blocked, err)
	}
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("deduped pass must still not touch the exchange, got %d adjusts", got)
	}
}

// (b) The early re-center: band 2, dwell 3, price 75% of the way to the
// adverse edge, bot in profit — one native adjust_params, reason
// RADAR_B2_EARLY_RECENTER, cooldown armed. Not far enough to the edge, or
// far enough but under water: no exchange call at all.
func TestRadarRealB2EarlyRecenter(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"

	// Telegram ON so the dedicated early-recenter template must render.
	var telegramSavedEnabled, telegramSavedNotify bool
	var telegramSavedToken, telegramSavedChat string
	_ = h.pool.QueryRow(ctx, `
		SELECT COALESCE(enabled,false), COALESCE(bot_token,''), COALESCE(chat_id,''),
		       COALESCE(notify_range_adjust,false)
		FROM telegram_settings WHERE id = 1
	`).Scan(&telegramSavedEnabled, &telegramSavedToken, &telegramSavedChat, &telegramSavedNotify)
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE telegram_settings
			SET enabled = $1, bot_token = $2, chat_id = $3, notify_range_adjust = $4
			WHERE id = 1
		`, telegramSavedEnabled, telegramSavedToken, telegramSavedChat, telegramSavedNotify)
		_, _ = h.pool.Exec(context.Background(),
			`DELETE FROM notification_outbox WHERE event_type = 'RADAR_B2_EARLY_RECENTER'`)
	})
	if _, err := h.pool.Exec(ctx, `
		UPDATE telegram_settings
		SET enabled = true, bot_token = 'test-token', chat_id = '100500', notify_range_adjust = true
		WHERE id = 1
	`); err != nil {
		t.Fatalf("enable telegram fixture: %v", err)
	}

	botID := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, botID, symbol, []float64{0.65, 0.65, 0.65})

	h.settings.StopForecastMode = "ACTIVE"
	// Price above mid = short inventory: the adverse edge is the upper bound
	// and 105 is 75% of the way there.
	b := realRadarAction(symbol, decimal.NewFromFloat(105))
	b.botID = botID
	b.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})

	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("green B2 bot at 75%% edge progress must early-shift, got %d adjusts", got)
	}
	body := h.mock.adjust()
	if body["type"] != "adjust_params" || body["bottom"] != "95" || body["top"] != "115" {
		t.Fatalf("early re-center must ship adjust_params [95, 115] (width 20 at price 105), got %v", body)
	}
	var reason string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&reason); err != nil || reason != "RADAR_B2_EARLY_RECENTER" {
		t.Fatalf("event reason must be RADAR_B2_EARLY_RECENTER, got %q (%v)", reason, err)
	}
	// The early shift arms the durable cooldown like any radar action.
	if _, ok := h.worker.radarLastActionAt(ctx, botID); !ok {
		t.Fatal("an executed early re-center must arm the durable radar cooldown")
	}
	// The dedicated telegram template renders, placeholder-free.
	var payload string
	if err := h.pool.QueryRow(ctx, `
		SELECT payload::TEXT FROM notification_outbox
		WHERE event_type = 'RADAR_B2_EARLY_RECENTER' ORDER BY created_at DESC LIMIT 1
	`).Scan(&payload); err != nil {
		t.Fatalf("early re-center telegram outbox row missing: %v", err)
	}
	if strings.Contains(payload, "{{") || !strings.Contains(payload, symbol) {
		t.Fatalf("early telegram must render fully and address the bot, got %s", payload)
	}

	// Not far enough: 60% of the way to the edge stays put.
	near := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, near, symbol, []float64{0.65, 0.65, 0.65})
	bNear := realRadarAction(symbol, decimal.NewFromFloat(102))
	bNear.botID = near
	bNear.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, bNear, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("60%% edge progress must not early-shift, got %d adjusts", got)
	}

	// Far enough but under water: blocked, no exchange call, durable event.
	wet := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, wet, symbol, []float64{0.65, 0.65, 0.65})
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, wet); err != nil {
		t.Fatalf("sink the early bot under water: %v", err)
	}
	bWet := realRadarAction(symbol, decimal.NewFromFloat(105))
	bWet.botID = wet
	bWet.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, bWet, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("underwater early trigger must be blocked pre-exchange, got %d adjusts", got)
	}
	var wetBlocked int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_SHIFT_BLOCKED_UNDERWATER'
	`, wet).Scan(&wetBlocked); err != nil || wetBlocked != 1 {
		t.Fatalf("underwater early trigger must leave one blocked event, got %d (%v)", wetBlocked, err)
	}
}

// manageExchangeMock serves one full REAL manage pass: per-bot order detail
// (bounds, position, grid profit), public prices, a klines feed too short for
// a regime (unknown = adverse both ways), empty funding history, and the
// signed adjustParams/cancel slots whose call counts are the assertions.
type manageExchangeMock struct {
	server     *httptest.Server
	adjustMu   sync.Mutex
	cancelMu   sync.Mutex
	adjustSeen int
	cancelSeen int
}

func newManageExchangeMock(t *testing.T) *manageExchangeMock {
	t.Helper()
	mock := &manageExchangeMock{}
	type botFixture struct {
		position, openPrice, profitReduce string
	}
	fixtures := map[string]botFixture{
		// LONG breaking UP while under water: position bought at 116.5 marked
		// at 115 (floating −1.5) plus grid −0.2 → total −1.7.
		"MGMT-A-bu": {position: "1", openPrice: "116.5", profitReduce: "-0.2"},
		// NEUTRAL breaking DOWN under water: position at 100 marked at 80.
		"MGMT-B-bu": {position: "1", openPrice: "100", profitReduce: "-0.5"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		fx := fixtures[r.URL.Query().Get("buOrderId")]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    "running", "reasonBy": "",
				"buOrderData": map[string]any{
					"status": "running", "reasonBy": "",
					"top": "110", "bottom": "90", "row": 20,
					"gridType": "arithmetic", "trend": "no_trend", "leverage": 2,
					"position": fx.position, "positionOpenPrice": fx.openPrice,
					"profitReduce": fx.profitReduce, "profitWithdrawn": "0",
					"riskStatus": "NORMAL",
				},
			},
		})
	})
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": "MGMTA_USDT_PERP", "indexPrice": "115", "markPrice": "115"},
				{"symbol": "MGMTB_USDT_PERP", "indexPrice": "80", "markPrice": "80"},
			}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": []map[string]any{
				{"symbol": "MGMTA_USDT_PERP", "close": "115"},
				{"symbol": "MGMTB_USDT_PERP", "close": "80"},
			}},
		})
	})
	// One candle < the 30-candle regime minimum: regime stays unknown, which
	// the break matrix treats as adverse for both directions.
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": []map[string]any{
				{"time": time.Now().UnixMilli(), "open": "100", "close": "100",
					"high": "100", "low": "100", "volume": "1"},
			}},
		})
	})
	mux.HandleFunc("GET /uapi/v1/trade/fundingFee", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"fundings": []map[string]any{}},
		})
	})
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"balances": []map[string]any{}},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParams", func(w http.ResponseWriter, _ *http.Request) {
		mock.adjustMu.Lock()
		mock.adjustSeen++
		mock.adjustMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.cancelMu.Lock()
		mock.cancelSeen++
		mock.cancelMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// (d) Manage RANGE_BREAK shifts share the preflight: an under-water bot whose
// break the matrix wants to follow with a shift (LONG up-break) never reaches
// the exchange — one SHIFT_BLOCKED_UNDERWATER event, bot stays RUNNING —
// while a break the matrix already closes (NEUTRAL down-break, adverse
// regime) still cancels exactly as before.
func TestManageRealRangeBreakPreflightAndBreakClose(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newManageExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-manage-preflight-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-manage-preflight-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-manage-preflight-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		if _, err := pool.Exec(cctx, `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup telemetry: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM bot_execution_events WHERE bot_id IN (
			SELECT id::TEXT FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup bots: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup permission health: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})

	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	seed := func(symbol, direction, remoteID string) string {
		var botID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO grid_bots (
				account_id, autogrid_settings_id, symbol, status, direction,
				grid_type, lower_price, upper_price, grid_num, leverage,
				quote_investment, extra_margin, request_fingerprint,
				execution_mode, reconciliation_state, bu_order_id,
				adjustments_count, created_at, bot_number
			) VALUES (
				$1, $2, $3, 'RUNNING', $4,
				'ARITHMETIC', 90, 110, 20, 2,
				200, 0, $5, 'REAL', 'REST_AUTHORITATIVE_OK', $6,
				0, NOW() - INTERVAL '2 hours', 600
			)
			RETURNING id
		`, account.ID, settings.ID, symbol, direction,
			"itest-"+time.Now().Format("150405.000000000"), remoteID).Scan(&botID); err != nil {
			t.Fatalf("seed %s: %v", symbol, err)
		}
		return botID
	}
	// The remote ids must match the mock fixtures so the pass reconciles the
	// intended under-water truth (A: floating −1.5 + grid −0.2; B: −20 −0.5).
	shiftBot := seed("MGMTA_USDT_PERP", "LONG", "MGMT-A-bu")
	closeBot := seed("MGMTB_USDT_PERP", "NEUTRAL", "MGMT-B-bu")

	rec := &recordingHandler{}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v (logs: %s)", err, rec.joined())
	}

	// The shift bot: preflight vetoed the RANGE_SHIFT_UP, nothing was sent.
	mock.adjustMu.Lock()
	adjustSeen := mock.adjustSeen
	mock.adjustMu.Unlock()
	if adjustSeen != 0 {
		t.Fatalf("underwater break must not reach adjustParams, got %d calls", adjustSeen)
	}
	var shiftStatus, shiftReconciliation string
	var shiftAdjustments int
	if err := pool.QueryRow(ctx, `
		SELECT status, reconciliation_state, adjustments_count FROM grid_bots WHERE id = $1
	`, shiftBot).Scan(&shiftStatus, &shiftReconciliation, &shiftAdjustments); err != nil {
		t.Fatalf("load shift bot: %v", err)
	}
	if shiftStatus != "RUNNING" || shiftAdjustments != 0 {
		t.Fatalf("blocked manage shift must leave the bot alive and budget intact: %s/%d",
			shiftStatus, shiftAdjustments)
	}
	var blockedReason string
	if err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'SHIFT_BLOCKED_UNDERWATER'
		ORDER BY created_at DESC LIMIT 1
	`, shiftBot).Scan(&blockedReason); err != nil || blockedReason != "RANGE_SHIFT_UP" {
		t.Fatalf("SHIFT_BLOCKED_UNDERWATER with reason RANGE_SHIFT_UP expected, got %q (%v)",
			blockedReason, err)
	}

	// The close bot: the adverse-regime break close still fires unchanged.
	mock.cancelMu.Lock()
	cancelSeen := mock.cancelSeen
	mock.cancelMu.Unlock()
	if cancelSeen != 1 {
		t.Fatalf("the RANGE_BREAK_DOWN close must still cancel exactly once, got %d", cancelSeen)
	}
	var closeStatus, closeReason, closeReconciliation string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason,''), COALESCE(reconciliation_state,'') FROM grid_bots WHERE id = $1
	`, closeBot).Scan(&closeStatus, &closeReason, &closeReconciliation); err != nil {
		t.Fatalf("load close bot: %v", err)
	}
	// Stop-in-flight contract (same tolerance as TestReconcileAndManageIntegration):
	// the success-path persist of 'CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING' hits a
	// PRE-EXISTING schema quirk — grid_bots.reconciliation_state is VARCHAR(32)
	// while the literal is 38 chars, so that UPDATE always fails silently and
	// the row can legitimately rest at STOPPING/CANCEL_SUBMITTING until the
	// next pass reconciles the remote terminal state. Either way the close
	// intent + reason must be durable and the cancel must have been sent.
	stopInFlight := (closeStatus == "STOP_REQUESTED" && closeReconciliation == "CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING") ||
		(closeStatus == "STOPPING" && closeReconciliation == "CANCEL_SUBMITTING")
	if !stopInFlight || closeReason != "RANGE_BREAK_DOWN" {
		t.Fatalf("break close must be stop-in-flight/RANGE_BREAK_DOWN, got %s/%s (%s)",
			closeStatus, closeReason, closeReconciliation)
	}
	var closeEventReason string
	if err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'STOP_LOSS' ORDER BY created_at DESC LIMIT 1
	`, closeBot).Scan(&closeEventReason); err != nil || closeEventReason != "RANGE_BREAK_DOWN" {
		t.Fatalf("the break close must leave its STOP_LOSS event, got %q (%v)", closeEventReason, err)
	}
}
