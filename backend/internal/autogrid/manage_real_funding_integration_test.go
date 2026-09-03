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

// fundingExchangeMock emulates the native Futures Grid order endpoints plus
// the signed funding-fee history endpoint used by the REAL funding
// reconciliation. Symbols map to fixed signed fee sets so each bot's expected
// cumulative sum is exact.
type fundingExchangeMock struct {
	mu           sync.Mutex
	fundingCalls map[string]int
	cancelCount  int
	fees         map[string][]map[string]any
	markPrice    map[string]string
	server       *httptest.Server
}

func newFundingExchangeMock(t *testing.T) *fundingExchangeMock {
	t.Helper()
	mock := &fundingExchangeMock{
		fundingCalls: map[string]int{},
		fees: map[string][]map[string]any{
			// Net -0.08 (received): funding_paid_usdt goes negative and must
			// RAISE realized, mirroring the paper receive branch.
			"FUNDA_USDT_PERP": {
				{"fundingFee": "-0.10", "timestamp": time.Now().UnixMilli()},
				{"fundingFee": "0.02", "timestamp": time.Now().UnixMilli()},
			},
			// Net +0.30 (paid): the classic drag — realized must drop.
			"FUNDB_USDT_PERP": {
				{"fundingFee": "0.30", "timestamp": time.Now().UnixMilli()},
			},
		},
		markPrice: map[string]string{
			"FUNDA_USDT_PERP": "110",
			"FUNDB_USDT_PERP": "220",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    "running", "reasonBy": "",
				"buOrderData": map[string]any{
					"status": "running", "reasonBy": "",
					"top": "120", "bottom": "100", "row": 20,
					"gridType": "arithmetic", "trend": "no_trend", "leverage": 1,
					"position": "0.5", "positionOpenPrice": "100",
					"profitWithdrawn": "1.5", "riskStatus": "NORMAL",
				},
			},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.cancelCount++
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("GET /uapi/v1/trade/fundingFee", func(w http.ResponseWriter, r *http.Request) {
		symbol := r.URL.Query().Get("symbol")
		if symbol == "" {
			t.Errorf("funding fetch must scope the query to the bot's symbol")
		}
		if r.URL.Query().Get("limit") != "200" {
			t.Errorf("expected documented limit 200, got %q", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("startTime") == "" || r.URL.Query().Get("endTime") == "" {
			t.Errorf("funding window must be bounded by startTime/endTime")
		}
		mock.mu.Lock()
		mock.fundingCalls[symbol]++
		fees := mock.fees[symbol]
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"fundings": fees},
		})
	})
	// Public mark prices so priceMap resolves both symbols and the manage pass
	// reaches telemetry and the decision ladder.
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		indexes := make([]map[string]any, 0, len(mock.markPrice))
		for symbol, mark := range mock.markPrice {
			indexes = append(indexes, map[string]any{
				"symbol": symbol, "indexPrice": mark, "markPrice": mark,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": indexes},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *fundingExchangeMock) callsFor(symbol string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fundingCalls[symbol]
}

func (m *fundingExchangeMock) cancels() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelCount
}

// TestManageRealFundingReconciliationIntegration proves the REAL manage pass
// books signed exchange funding into the durable column and realized PnL, and
// that the 30-minute anchor keeps a second pass off the private endpoint.
func TestManageRealFundingReconciliationIntegration(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newFundingExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-funding-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-funding-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-funding-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-funding-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-funding-test%'`)
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

	// Two RUNNING REAL bots on distinct symbols with far-away target/loss so
	// the decision ladder stays HOLD and funding is the only PnL delta.
	botIDs := make([]string, 0, 2)
	for _, symbol := range []string{"FUNDA_USDT_PERP", "FUNDB_USDT_PERP"} {
		var botID string
		lower, upper := "100", "120"
		if symbol == "FUNDB_USDT_PERP" {
			lower, upper = "200", "240"
		}
		err = pool.QueryRow(ctx, `
			INSERT INTO grid_bots (
				account_id, autogrid_settings_id, symbol, status, direction,
				grid_type, lower_price, upper_price, grid_num, leverage,
				quote_investment, extra_margin, request_fingerprint,
				execution_mode, reconciliation_state, bu_order_id,
				pnl_target_usdt, max_loss_usdt
			) VALUES (
				$1, $2, $3, 'RUNNING', 'NEUTRAL',
				'ARITHMETIC', $4, $5, 20, 1,
				100, 0, $6, 'REAL', 'REMOTE_ID_PERSISTED', $7,
				1000, 1000
			)
			RETURNING id
		`, account.ID, settings.ID, symbol, lower, upper,
			"itest-"+time.Now().Format("150405.000000000"),
			"FUND-"+symbol,
		).Scan(&botID)
		if err != nil {
			t.Fatalf("insert grid bot %s: %v", symbol, err)
		}
		botIDs = append(botIDs, botID)
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	// Public market data must also come from the mock: the real host is not
	// reachable from the test environment and mark prices drive the pass.
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}

	// fundingPaid maps symbol -> expected cumulative signed sum.
	fundingPaid := map[string]decimal.Decimal{
		"FUNDA_USDT_PERP": decimal.RequireFromString("-0.08"),
		"FUNDB_USDT_PERP": decimal.RequireFromString("0.30"),
	}
	for i, symbol := range []string{"FUNDA_USDT_PERP", "FUNDB_USDT_PERP"} {
		botID := botIDs[i]
		var fundingPaidCol, realized string
		var anchor *time.Time
		var status string
		if err := pool.QueryRow(ctx, `
			SELECT funding_paid_usdt::TEXT, realized_pnl_usdt::TEXT,
			       last_funding_reconcile_at, status
			FROM grid_bots WHERE id = $1
		`, botID).Scan(&fundingPaidCol, &realized, &anchor, &status); err != nil {
			t.Fatalf("load bot %s: %v", symbol, err)
		}
		if status != "RUNNING" {
			t.Fatalf("%s: bot must stay RUNNING on a HOLD decision, got %s", symbol, status)
		}
		if anchor == nil {
			t.Fatalf("%s: last_funding_reconcile_at must be set after the pass", symbol)
		}
		if got := decimal.RequireFromString(fundingPaidCol); !got.Equal(fundingPaid[symbol]) {
			t.Fatalf("%s: funding_paid_usdt = %s, want %s", symbol, fundingPaidCol, fundingPaid[symbol])
		}
		// Remote truth 1.5 minus the signed column: paid (positive) drags
		// realized down, received (negative) lifts it — the paper mirror.
		wantRealized := decimal.NewFromFloat(1.5).Sub(fundingPaid[symbol])
		if got := decimal.RequireFromString(realized); !got.Equal(wantRealized) {
			t.Fatalf("%s: realized_pnl_usdt = %s, want %s", symbol, realized, wantRealized)
		}
		var telemetryFunding, telemetryRealized string
		if err := pool.QueryRow(ctx, `
			SELECT funding_paid_usdt::TEXT, realized_pnl::TEXT
			FROM bot_telemetry WHERE bot_id = $1
			ORDER BY captured_at DESC LIMIT 1
		`, botID).Scan(&telemetryFunding, &telemetryRealized); err != nil {
			t.Fatalf("%s: telemetry row missing: %v", symbol, err)
		}
		if got := decimal.RequireFromString(telemetryFunding); !got.Equal(fundingPaid[symbol]) {
			t.Fatalf("%s: telemetry funding = %s, want %s", symbol, telemetryFunding, fundingPaid[symbol])
		}
		if got := decimal.RequireFromString(telemetryRealized); !got.Equal(wantRealized) {
			t.Fatalf("%s: telemetry realized = %s, want %s", symbol, telemetryRealized, wantRealized)
		}
		if got := mock.callsFor(symbol); got != 1 {
			t.Fatalf("%s: expected exactly one funding fetch, got %d", symbol, got)
		}
	}
	if got := mock.cancels(); got != 0 {
		t.Fatalf("HOLD decision must not cancel any bot, got %d cancels", got)
	}

	// Pass 2 within the 30-minute guard: the anchor must keep the private
	// funding endpoint untouched and the booking idempotent.
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	for _, symbol := range []string{"FUNDA_USDT_PERP", "FUNDB_USDT_PERP"} {
		if got := mock.callsFor(symbol); got != 1 {
			t.Fatalf("%s: 30-minute guard violated — %d funding fetches", symbol, got)
		}
	}
	for i, symbol := range []string{"FUNDA_USDT_PERP", "FUNDB_USDT_PERP"} {
		var fundingPaidCol, realized string
		if err := pool.QueryRow(ctx, `
			SELECT funding_paid_usdt::TEXT, realized_pnl_usdt::TEXT
			FROM grid_bots WHERE id = $1
		`, botIDs[i]).Scan(&fundingPaidCol, &realized); err != nil {
			t.Fatalf("load bot %s after pass 2: %v", symbol, err)
		}
		if got := decimal.RequireFromString(fundingPaidCol); !got.Equal(fundingPaid[symbol]) {
			t.Fatalf("%s: funding drifted after pass 2: %s", symbol, fundingPaidCol)
		}
		wantRealized := decimal.NewFromFloat(1.5).Sub(fundingPaid[symbol])
		if got := decimal.RequireFromString(realized); !got.Equal(wantRealized) {
			t.Fatalf("%s: realized drifted after pass 2: %s, want %s", symbol, realized, wantRealized)
		}
	}
}
