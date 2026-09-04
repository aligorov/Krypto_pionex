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

// pnlExchangeMock serves the wire shapes the v2.0.74 PnL contract needs:
// a running grid whose profit lives in profitReduce with per-bot
// fundingFeePayment; a grid the detail endpoint reports as terminal with a
// settled profitExited; and a grid the detail endpoint refuses (404) whose
// final record only exists in the finished-bot list.
type pnlExchangeMock struct {
	mu           sync.Mutex
	fundingCalls int
	cancelCount  int
	server       *httptest.Server
}

func newPnlExchangeMock(t *testing.T) *pnlExchangeMock {
	t.Helper()
	mock := &pnlExchangeMock{}
	mux := http.NewServeMux()
	gridPayload := func(status, reason, exited, reduce, funding string) map[string]any {
		return map[string]any{
			"status": status, "reasonBy": reason,
			"top": "1.8", "bottom": "1.4", "row": 20,
			"gridType": "arithmetic", "trend": "no_trend", "leverage": 1,
			"position": "2", "positionOpenPrice": "1.5",
			"profitReduce": reduce, "profitWithdrawn": "0",
			"profitExited": exited, "fundingFeePayment": funding,
			"riskStatus": "NORMAL",
		}
	}
	respond := func(w http.ResponseWriter, payload any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("buOrderId") {
		case "PNL-A":
			respond(w, map[string]any{
				"result": true, "timestamp": time.Now().UnixMilli(),
				"data": map[string]any{
					"buOrderId": "PNL-A", "status": "running", "reasonBy": "",
					"buOrderData": gridPayload("running", "", "0", "0.087", "-0.02"),
				},
			})
		case "PNL-B":
			respond(w, map[string]any{
				"result": true, "timestamp": time.Now().UnixMilli(),
				"data": map[string]any{
					"buOrderId": "PNL-B", "status": "canceled", "reasonBy": "profit_stop",
					"buOrderData": gridPayload("canceled", "profit_stop", "0.42", "0.10", "-0.01"),
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			respond(w, map[string]any{
				"result": false, "timestamp": time.Now().UnixMilli(),
				"code": "P_TRADING_BOT_ORDER_NOT_FOUND", "message": "order not found",
			})
		}
	})
	// The finished-bot list is the only place the refused grid's final
	// record survives; the detail endpoint's 404 must lead here.
	mux.HandleFunc("GET /api/v1/bot/orders", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "finished" {
			respond(w, map[string]any{
				"result": true, "timestamp": time.Now().UnixMilli(),
				"data": map[string]any{"orders": []any{}},
			})
			return
		}
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"orders": []map[string]any{{
				"buOrderId": "PNL-C", "buOrderType": "futures_grid", "status": "finished",
				"base": "PNLC", "quote": "USDT", "createTime": time.Now().UnixMilli(),
				"buOrderData": gridPayload("canceled", "user_cancel", "0.31", "0.05", "0"),
			}}},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.cancelCount++
		mock.mu.Unlock()
		respond(w, map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	// The per-bot fundingFeePayment on every grid record must keep the
	// symbol-wide history endpoint untouched.
	mux.HandleFunc("GET /uapi/v1/trade/fundingFee", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.fundingCalls++
		mock.mu.Unlock()
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"fundings": []any{}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": "PNLA_USDT_PERP", "indexPrice": "1.6", "markPrice": "1.6"},
				{"symbol": "PNLB_USDT_PERP", "indexPrice": "1.6", "markPrice": "1.6"},
				{"symbol": "PNLC_USDT_PERP", "indexPrice": "1.6", "markPrice": "1.6"},
			}},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *pnlExchangeMock) fundingFetches() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fundingCalls
}

func (m *pnlExchangeMock) cancels() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelCount
}

// TestReconcilePnLMappingIntegration proves the REAL reconcile persists the
// exchange's own PnL truth end to end:
//
//	running: realized = profitReduce + fundingFeePayment, unrealized =
//	         position×(mark−open), funding column resynced per-bot, no
//	         symbol-wide funding fetch;
//	terminal: realized = settled profitExited, floating zeroed;
//	ALREADY_CLOSED (detail endpoint 404): realized = profitExited from the
//	         finished-bot list, stale floating zeroed.
func TestReconcilePnLMappingIntegration(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newPnlExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-pnl-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-pnl-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-pnl-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-pnl-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-pnl-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup telemetry: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup grid bots: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup permission health: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})

	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
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
		if _, err := pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4, ai_autotune_enabled = $5
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode, savedAutotune); err != nil {
			t.Errorf("restore settings failed — stand state may be dirty: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, status = 'RUNNING', execution_mode = 'REAL',
		    ai_autotune_enabled = false
		WHERE scope_key = $1::VARCHAR
	`, DefaultScope, account.ID); err != nil {
		t.Fatalf("retarget settings: %v", err)
	}

	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	seedBot := func(symbol, remoteID string, staleUnrealized string) string {
		t.Helper()
		var botID string
		err := pool.QueryRow(ctx, `
			INSERT INTO grid_bots (
				account_id, autogrid_settings_id, symbol, status, direction,
				grid_type, lower_price, upper_price, grid_num, leverage,
				quote_investment, extra_margin, request_fingerprint,
				execution_mode, reconciliation_state, bu_order_id,
				pnl_target_usdt, max_loss_usdt, unrealized_pnl_usdt
			) VALUES (
				$1, $2, $3, 'RUNNING', 'NEUTRAL',
				'ARITHMETIC', 1.4, 1.8, 20, 1,
				100, 0, $4, 'REAL', 'REMOTE_ID_PERSISTED', $5,
				1000, 1000, $6::NUMERIC
			)
			RETURNING id
		`, account.ID, settings.ID, symbol,
			"itest-"+time.Now().Format("150405.000000000")+"-"+symbol,
			remoteID, staleUnrealized,
		).Scan(&botID)
		if err != nil {
			t.Fatalf("insert grid bot %s: %v", symbol, err)
		}
		return botID
	}
	runningID := seedBot("PNLA_USDT_PERP", "PNL-A", "0")
	terminalID := seedBot("PNLB_USDT_PERP", "PNL-B", "0")
	// The already-closed bot still carries its last (negative) floating mark
	// from before the exchange finished it — the reconcile must zero it.
	closedID := seedBot("PNLC_USDT_PERP", "PNL-C", "-0.5")

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	assertBot := func(t *testing.T, botID, wantStatus, wantClosedReason string,
		wantRealized, wantUnrealized, wantFunding string) {
		t.Helper()
		var status, closedReason, realized, unrealized, funding string
		if err := pool.QueryRow(ctx, `
			SELECT status, COALESCE(closed_reason,''),
			       COALESCE(realized_pnl_usdt,0)::TEXT,
			       COALESCE(unrealized_pnl_usdt,0)::TEXT,
			       COALESCE(funding_paid_usdt,0)::TEXT
			FROM grid_bots WHERE id = $1
		`, botID).Scan(&status, &closedReason, &realized, &unrealized, &funding); err != nil {
			t.Fatalf("load bot %s: %v", botID, err)
		}
		if status != wantStatus {
			t.Fatalf("bot %s status = %s, want %s", botID, status, wantStatus)
		}
		if closedReason != wantClosedReason {
			t.Fatalf("bot %s closed_reason = %q, want %q", botID, closedReason, wantClosedReason)
		}
		if got := decimal.RequireFromString(realized); !got.Equal(decimal.RequireFromString(wantRealized)) {
			t.Fatalf("bot %s realized = %s, want %s", botID, realized, wantRealized)
		}
		if got := decimal.RequireFromString(unrealized); !got.Equal(decimal.RequireFromString(wantUnrealized)) {
			t.Fatalf("bot %s unrealized = %s, want %s", botID, unrealized, wantUnrealized)
		}
		if got := decimal.RequireFromString(funding); !got.Equal(decimal.RequireFromString(wantFunding)) {
			t.Fatalf("bot %s funding_paid = %s, want %s", botID, funding, wantFunding)
		}
	}

	// Running grid: grid profit 0.087 + per-bot funding −0.02 → 0.067;
	// floating = 2 × (1.6 − 1.5) = 0.2 — the app's Total PnL would be 0.267.
	assertBot(t, runningID, "RUNNING", "", "0.067", "0.2", "0.02")
	// Terminal grid: settled profitExited 0.42, no floating left.
	assertBot(t, terminalID, "COMPLETED", "TAKE_PROFIT_NATIVE", "0.42", "0", "0.01")
	// Refused detail endpoint: final profit 0.31 from the finished list and
	// the stale −0.5 floating zeroed instead of lingering forever.
	assertBot(t, closedID, "STOPPED", "ALREADY_CLOSED", "0.31", "0", "0")

	if got := mock.fundingFetches(); got != 0 {
		t.Fatalf("per-bot fundingFeePayment must keep the symbol-wide funding endpoint untouched, got %d fetches", got)
	}
	if got := mock.cancels(); got != 0 {
		t.Fatalf("no bot in this scenario may be cancelled, got %d cancels", got)
	}
}
