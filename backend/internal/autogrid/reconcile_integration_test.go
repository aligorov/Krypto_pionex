package autogrid

import (
	"context"
	"encoding/json"
	"strconv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationDatabaseURL gates tests that need a real PostgreSQL. They run
// against a disposable schema state and restore everything they touch.
func integrationDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PIONEX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PIONEX_TEST_DATABASE_URL is not set; skipping integration test")
	}
	return url
}

// exchangeMock emulates the native Pionex Futures Grid endpoints for one bot:
// it reports a running bot with position and withdrawn profit, flips to
// canceled/profit_stop after the cancel call, and counts cancels.
type exchangeMock struct {
	cancelCount atomic.Int64
	terminal    atomic.Bool
	server      *httptest.Server
}

func newExchangeMock(t *testing.T) *exchangeMock {
	t.Helper()
	mock := &exchangeMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		status, reason := "running", ""
		if mock.terminal.Load() {
			status, reason = "canceled", "profit_stop"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    status, "reasonBy": reason,
				"buOrderData": map[string]any{
					"status": status, "reasonBy": reason,
					"top": "120", "bottom": "100", "row": 20,
					"gridType": "arithmetic", "trend": "no_trend", "leverage": 1,
					"position": "0.5", "positionOpenPrice": "100",
					"profitWithdrawn": "1.5", "riskStatus": "NORMAL",
				},
			},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.cancelCount.Add(1)
		mock.terminal.Store(true)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// TestReconcileAndManageIntegration drives the full supervision loop against
// a mock exchange: a stop intent must reach the exchange as a native cancel,
// remote PnL must be persisted, and the terminal state must map to
// COMPLETED/TAKE_PROFIT_NATIVE.
func TestReconcileAndManageIntegration(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs last: deferred Close would execute before
	// the t.Cleanup restores below and every restore would hit a closed pool.
	t.Cleanup(pool.Close)

	mock := newExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	// Dedicated verified account whose client talks to the mock exchange.
	accountName := "integration-reconcile-test-" + time.Now().Format("150405.000000000")
	// Detach any stale test account the settings may reference, then remove
	// leftovers from earlier runs (order matters: FK from autogrid_settings).
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-reconcile-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-reconcile-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-reconcile-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-reconcile-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
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

	// Point the account's cached client at the mock by planting the mock URL
	// as the base through the client cache with a fixed fingerprint.
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	// Snapshot and retarget the default settings row; restore afterwards.
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

	var botID string
	err = pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id
		) VALUES (
			$1, $2, 'TEST_USDT_PERP', 'STOP_REQUESTED', 'NEUTRAL',
			'ARITHMETIC', 100, 120, 20, 1,
			100, 0, $3, 'REAL', 'REMOTE_ID_PERSISTED', 'ITEST-1'
		)
		RETURNING id
	`, account.ID, settings.ID, "itest-"+time.Now().Format("150405.000000000")).Scan(&botID)
	if err != nil {
		t.Fatalf("insert grid bot: %v", err)
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	// Round 1: the durable stop intent must submit the native cancel.
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile round 1: %v", err)
	}
	if got := mock.cancelCount.Load(); got != 1 {
		t.Fatalf("expected exactly one native cancel, got %d", got)
	}
	var status, reconciliation string
	var realized *string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(reconciliation_state,''), realized_pnl_usdt::TEXT
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&status, &reconciliation, &realized); err != nil {
		t.Fatalf("load bot after round 1: %v", err)
	}
	if realized == nil {
		t.Fatal("realized PnL must be persisted")
	}
	if value, err := strconv.ParseFloat(*realized, 64); err != nil || value != 1.5 {
		t.Fatalf("remote profitWithdrawn must persist as realized PnL 1.5, got %q", *realized)
	}
	if status != "STOP_REQUESTED" && status != "STOPPING" {
		t.Fatalf("bot must stay in stop-in-flight status, got %s", status)
	}

	// Round 2: mock now reports canceled with reasonBy=profit_stop.
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile round 2: %v", err)
	}
	var finalStatus, closedReason string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason,'') FROM grid_bots WHERE id = $1
	`, botID).Scan(&finalStatus, &closedReason); err != nil {
		t.Fatalf("load bot after round 2: %v", err)
	}
	if finalStatus != "COMPLETED" || closedReason != "TAKE_PROFIT_NATIVE" {
		t.Fatalf("expected COMPLETED/TAKE_PROFIT_NATIVE, got %s/%s", finalStatus, closedReason)
	}
	if got := mock.cancelCount.Load(); got != 1 {
		t.Fatalf("terminal verification must not re-cancel, got %d cancels", got)
	}
}
