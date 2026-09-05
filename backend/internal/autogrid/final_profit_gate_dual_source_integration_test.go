package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestGateSettledProfitDualSource is the unit contract of the v2.0.89 sanity
// gate: the exchange-total legs (profitExited / total-alias) pass for every
// close class EXCEPT the guarded anomaly — a POSITIVE total on a loss-class
// close, decided from BOTH the stored closed_reason and the exchange's
// reasonBy. Every other source (the retired grid+funding legs, withdrawn,
// none) answers nil: those figures are never finals, the caller falls to the
// telemetry-net-close estimate.
func TestGateSettledProfitDualSource(t *testing.T) {
	positive := decimal.NewFromFloat(0.2)
	negative := decimal.NewFromFloat(-2.5)

	// The prod FARTCOIN shape, on the TOTAL leg now: our manage stop
	// (STOP_LOSS stored) comes back from the exchange as "user cancel" — the
	// operator-class exchange reason alone must NOT launder the loss-class
	// close open.
	if got := gateSettledProfit(positive, pionex.FinalProfitTotalAlias, "STOP_LOSS", "user cancel"); got != nil {
		t.Fatalf("positive total-alias on a stored STOP_LOSS must refuse (user cancel at the exchange), got %s", *got)
	}
	if got := gateSettledProfit(positive, pionex.FinalProfitExited, "STOP_LOSS", "user cancel"); got != nil {
		t.Fatalf("positive profitExited on a stored STOP_LOSS must refuse, got %s", *got)
	}
	// Either reason source alone suffices.
	if got := gateSettledProfit(positive, pionex.FinalProfitTotalAlias, "", "loss_stop"); got != nil {
		t.Fatalf("positive total-alias on exchange loss_stop must refuse, got %s", *got)
	}
	if got := gateSettledProfit(positive, pionex.FinalProfitTotalAlias, "RANGE_SHIFT_DOWN_NO_ADJUSTMENTS_LEFT", ""); got != nil {
		t.Fatalf("positive total-alias on RANGE_SHIFT_*_NO_ADJUSTMENTS_LEFT must refuse, got %s", *got)
	}
	// A KNOWN non-loss class keeps the exchange total.
	if got := gateSettledProfit(positive, pionex.FinalProfitTotalAlias, "AUTOGRID_STOP", "user cancel"); got == nil || !got.Equal(positive) {
		t.Fatalf("positive total on a known operator-class close must stay accepted, got %v", got)
	}
	// Negative totals always pass — the gate refuses lies, never losses.
	if got := gateSettledProfit(negative, pionex.FinalProfitExited, "STOP_LOSS", "loss_stop"); got == nil || !got.Equal(negative) {
		t.Fatalf("profitExited −2.5 must settle −2.5, got %v", got)
	}
	// The retired legs are not the gate's business: nil for any figure.
	for _, source := range []pionex.FinalProfitSource{
		pionex.FinalProfitGridResidual, pionex.FinalProfitGridFlat,
		pionex.FinalProfitWithdrawn, pionex.FinalProfitNone,
	} {
		if got := gateSettledProfit(negative, source, "STOP_LOSS", "loss_stop"); got != nil {
			t.Fatalf("non-total source %s must never gate a final, got %s", source, *got)
		}
		if got := gateSettledProfit(positive, source, "AUTOGRID_STOP", "user cancel"); got != nil {
			t.Fatalf("non-total source %s must never gate a final, got %s", source, *got)
		}
	}
}

func TestLossClassCloseDictionary(t *testing.T) {
	loss := []string{
		"STOP_LOSS", "STOP_LOSS_NATIVE", "LIQUIDATION", "FORCE_LIQUIDATION",
		"STRUCT_INVALID_ANTI_HUNT", "RANGE_BREAK_DOWN", "RANGE_BREAK_UP",
		"RANGE_BREAK_UP_TREND_STOP", "LOSS_STOP",
		"RANGE_SHIFT_DOWN_NO_ADJUSTMENTS_LEFT", "RANGE_SHIFT_UP_NO_ADJUSTMENTS_LEFT",
		" loss_stop ", "range_break_up",
	}
	for _, reason := range loss {
		if !lossClassClose(reason) {
			t.Fatalf("%q must classify as loss-class", reason)
		}
	}
	notLoss := []string{
		"", "TAKE_PROFIT", "TAKE_PROFIT_NATIVE", "TRAILING_TAKE_PROFIT",
		"USER_CANCEL", "MANUAL_CLOSE", "AUTOGRID_STOP", "EXTERNAL_CLOSE",
		"RANGE_SHIFT_DOWN", "RANGE_BREAK_UP_PROFIT_TAKE",
	}
	for _, reason := range notLoss {
		if lossClassClose(reason) {
			t.Fatalf("%q must NOT classify as loss-class", reason)
		}
	}
	for _, reason := range []string{"", "ALREADY_CLOSED", " already_closed "} {
		if !unknownClassClose(reason) {
			t.Fatalf("%q must classify as unknown close class", reason)
		}
	}
	for _, reason := range []string{"STOP_LOSS", "TAKE_PROFIT", "EXTERNAL_CLOSE"} {
		if unknownClassClose(reason) {
			t.Fatalf("%q is a known close class", reason)
		}
	}
}

// terminalGateExchangeMock serves the futures-grid detail endpoint in the
// FARTCOIN production shape: a bot our manage loop stopped (native cancel)
// reports terminal "canceled" with the exchange reasonBy "user cancel".
type terminalGateExchangeMock struct {
	server *httptest.Server
	mu     sync.Mutex
	// profitExited, when non-empty, makes SettledProfit return the full-chain
	// figure; otherwise profitReduce + closedBaseAmount produce a positive
	// grid_funding_residual.
	profitExited  string
	profitReduce  string
	terminalFlips bool
}

func newTerminalGateExchangeMock(t *testing.T) *terminalGateExchangeMock {
	t.Helper()
	mock := &terminalGateExchangeMock{profitReduce: "0.2"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		exited, reduce, flip := mock.profitExited, mock.profitReduce, mock.terminalFlips
		mock.mu.Unlock()
		status, reason := "running", ""
		if flip {
			status, reason = "canceled", "user cancel"
		}
		bu := map[string]any{
			"status": status, "reasonBy": reason,
			"top": "110", "bottom": "90", "row": 20,
			"gridType": "arithmetic", "trend": "no_trend", "leverage": 2,
			"position": "0.4", "positionOpenPrice": "100",
			"profitReduce": reduce, "profitWithdrawn": "0",
			"closedBaseAmount": "1.4", "fundingFeePayment": "0",
			"riskStatus": "NORMAL",
		}
		if exited != "" {
			bu["profitExited"] = exited
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    status, "reasonBy": reason,
				"buOrderData": bu,
			},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.terminalFlips = true
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"balances": []map[string]any{}},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// seedGateBot lands one STOP_REQUESTED REAL bot whose closed_reason is the
// local manage-stop witness; the exchange answers "user cancel".
func seedGateBot(t *testing.T, pool *pgxpool.Pool, accountID, settingsID, symbol, buOrderID, closedReason string, maxLoss any) string {
	t.Helper()
	var botID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			realized_pnl_usdt, unrealized_pnl_usdt, closed_reason, max_loss_usdt, bot_number
		) VALUES (
			$1, $2, $3, 'STOP_REQUESTED', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $4, 'REAL', 'REMOTE_ID_PERSISTED', $5,
			0, 0, $6, $7, 888
		)
		RETURNING id
	`, accountID, settingsID, symbol,
		"itest-"+time.Now().Format("150405.000000000"), buOrderID, closedReason, maxLoss).Scan(&botID); err != nil {
		t.Fatalf("seed gate bot %s: %v", symbol, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM bot_telemetry WHERE bot_id = $1::TEXT`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM bot_execution_events WHERE bot_id = $1::TEXT`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM grid_bots WHERE id = $1`, botID)
	})
	return botID
}

// seedGateTelemetry plants the last telemetry row the estimate ladder reads.
func seedGateTelemetry(t *testing.T, pool *pgxpool.Pool, botID, total, inventory string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO bot_telemetry
			(bot_id, bot_number, symbol, captured_at, price, total_pnl, inventory_notional)
		VALUES ($1, 888, 'GATE', NOW() - INTERVAL '30 seconds', 100, $2::NUMERIC, $3::NUMERIC)
	`, botID, total, inventory); err != nil {
		t.Fatalf("seed gate telemetry: %v", err)
	}
}

// TestTerminalSettleGateConsultsStoredReason drives the terminal settle path
// end to end under the v2.0.89 ladder:
//
//	Case 1 — a bot our manage loop stopped (stored STOP_LOSS, exchange answers
//	"user cancel") whose record carries ONLY grid profit (no netted total):
//	the retired residual leg is not a final; the telemetry-net-close estimate
//	settles instead, and its close cost is booked into fees_paid_usdt.
//
//	Case 2 — the same bot shape with the full-chain profitExited −2.5 settles
//	−2.5 (the gate refuses incomplete legs, never the exchange's own net).
//
//	Case 3 — the cascade shape: a NATIVE stop whose last telemetry mark still
//	read positive while the stop executed; the estimate floors at
//	−max_loss − close_cost (prod APT: stop executed at −12 with the last mark
//	at −2.27, ICP/SUI: stops fired on positive marks).
func TestTerminalSettleGateConsultsStoredReason(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newTerminalGateExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-gate-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-gate-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-gate-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-gate-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{fingerprint: account.KeyFingerprint, client: mockClient}
	service.clientMu.Unlock()

	var savedAccountID *string
	var savedStatus, savedMode string
	var savedAutotune bool
	if err := pool.QueryRow(ctx, `
		SELECT account_id, status, execution_mode, ai_autotune_enabled
		FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID, &savedStatus, &savedMode, &savedAutotune); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4, ai_autotune_enabled = $5
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode, savedAutotune)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, status = 'RUNNING', execution_mode = 'REAL', ai_autotune_enabled = false
		WHERE scope_key = $1::VARCHAR
	`, DefaultScope, account.ID); err != nil {
		t.Fatalf("retarget settings: %v", err)
	}
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	rec := &recordingHandler{}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	// Case 1 (prod FARTCOIN): stored STOP_LOSS + exchange "user cancel" +
	// residual grid profit +0.2 (no netted total) + telemetry total −1.0 on
	// 200 inventory → estimate −1.0 − 0.2 = −1.2, close cost booked.
	residualID := seedGateBot(t, pool, account.ID, settings.ID,
		"GATEA_USDT_PERP", "GATE-A", "STOP_LOSS", nil)
	seedGateTelemetry(t, pool, residualID, "-1.0", "200")
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}
	var realized *decimal.Decimal
	var marker string
	var status1, closed1 string
	var feesPaid decimal.Decimal
	var closeCostState *decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt, COALESCE(model_state->>'finalProfitSource',''),
		       status, COALESCE(closed_reason,''), fees_paid_usdt,
		       NULLIF(model_state->>'closeCostUsdt','')::NUMERIC
		FROM grid_bots WHERE id = $1
	`, residualID).Scan(&realized, &marker, &status1, &closed1, &feesPaid, &closeCostState); err != nil {
		t.Fatalf("load residual bot: %v", err)
	}
	if realized == nil || !realized.Equal(decimal.NewFromFloat(-1.2)) {
		t.Fatalf("no exchange total → telemetry_net_close = −1.0 − 0.001×200 = −1.2, got %v", realized)
	}
	if marker != string(pionex.FinalProfitTelemetryNetClose) {
		t.Fatalf("estimate settle must mark telemetry_net_close, got %q", marker)
	}
	if !feesPaid.Equal(decimal.NewFromFloat(0.2)) {
		t.Fatalf("close cost 0.2 must be booked into fees_paid_usdt, got %s", feesPaid)
	}
	if closeCostState == nil || !closeCostState.Equal(decimal.NewFromFloat(0.2)) {
		t.Fatalf("close cost must ride in model_state.closeCostUsdt, got %v", closeCostState)
	}
	if status1 != "STOPPED" || closed1 != "STOP_LOSS" {
		t.Fatalf("stored closed_reason must survive the terminal persist, got %s/%s", status1, closed1)
	}

	// Case 2: the same bot shape with the full-chain figure −2.5 settles −2.5
	// (the gate refuses incomplete legs, never the exchange's own net).
	exitedID := seedGateBot(t, pool, account.ID, settings.ID,
		"GATEB_USDT_PERP", "GATE-B", "STOP_LOSS", nil)
	mock.mu.Lock()
	mock.profitExited = "-2.5"
	mock.mu.Unlock()
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	var exitedRealized decimal.Decimal
	var exitedMarker string
	var exitedFees decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt, COALESCE(model_state->>'finalProfitSource',''), fees_paid_usdt
		FROM grid_bots WHERE id = $1
	`, exitedID).Scan(&exitedRealized, &exitedMarker, &exitedFees); err != nil {
		t.Fatalf("load exited bot: %v", err)
	}
	if !exitedRealized.Equal(decimal.NewFromFloat(-2.5)) {
		t.Fatalf("profitExited −2.5 must settle −2.5, got %s", exitedRealized)
	}
	if exitedMarker != string(pionex.FinalProfitExited) {
		t.Fatalf("settle must mark profit_exited, got %q", exitedMarker)
	}
	if !exitedFees.IsZero() {
		t.Fatalf("an exchange-total settle books no close cost (already netted), got %s", exitedFees)
	}

	// Case 3: the cascade shape — native stop, stale positive mark, the floor
	// at −max_loss − close_cost carries the mark-to-execution gap.
	mock.mu.Lock()
	mock.profitExited = ""
	mock.mu.Unlock()
	cascadeID := seedGateBot(t, pool, account.ID, settings.ID,
		"GATEC_USDT_PERP", "GATE-C", "STOP_LOSS_NATIVE", "4")
	seedGateTelemetry(t, pool, cascadeID, "0.5", "100")
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 3: %v", err)
	}
	var cascadeRealized *decimal.Decimal
	var cascadeMarker string
	var cascadeFees decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt, COALESCE(model_state->>'finalProfitSource',''), fees_paid_usdt
		FROM grid_bots WHERE id = $1
	`, cascadeID).Scan(&cascadeRealized, &cascadeMarker, &cascadeFees); err != nil {
		t.Fatalf("load cascade bot: %v", err)
	}
	if cascadeRealized == nil || !cascadeRealized.Equal(decimal.NewFromFloat(-4.1)) {
		t.Fatalf("native stop on a positive mark must floor at −max_loss − close_cost = −4.1, got %v", cascadeRealized)
	}
	if cascadeMarker != string(pionex.FinalProfitTelemetryNetClose) {
		t.Fatalf("floored estimate must mark telemetry_net_close, got %q", cascadeMarker)
	}
	if !cascadeFees.Equal(decimal.NewFromFloat(0.1)) {
		t.Fatalf("cascade close cost 0.1 must be booked, got %s", cascadeFees)
	}
}
