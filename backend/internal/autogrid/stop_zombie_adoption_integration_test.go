package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/grid"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// adoptionExchangeMock serves the endpoints one adoption manage pass touches:
// a running helper bot on the detail endpoint, prices, the funding feed, and
// a configurable GET /api/v1/bot/orders list (the documented 'results' key).
type adoptionExchangeMock struct {
	server *httptest.Server
	mu     sync.Mutex
	orders []map[string]any
}

func (m *adoptionExchangeMock) setOrders(orders []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orders = orders
}

func newAdoptionExchangeMock(t *testing.T, helperSymbol, price string) *adoptionExchangeMock {
	t.Helper()
	mock := &adoptionExchangeMock{}
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
					"top": "110", "bottom": "90", "row": 20,
					"gridType": "arithmetic", "trend": "no_trend", "leverage": 2,
					"position": "0", "positionOpenPrice": price,
					"profitReduce": "0.1", "profitWithdrawn": "0",
					"riskStatus": "NORMAL",
				},
			},
		})
	})
	mux.HandleFunc("GET /api/v1/bot/orders", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		orders := mock.orders
		mock.mu.Unlock()
		// The reconcile paginates BOTH lists; an order appearing in two lists
		// would double-match and turn adoption ambiguous. Only the running
		// list carries the fixtures.
		if r.URL.Query().Get("status") != "running" {
			orders = nil
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"results": orders},
		})
	})
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": helperSymbol, "indexPrice": price, "markPrice": price},
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
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// seedUnknownSubmission lands a REAL row with bu_order_id IS NULL in any of
// the submission statuses — the exact rows the CRIT-3 guards reason about.
func seedUnknownSubmission(t *testing.T, pool *pgxpool.Pool, accountID, settingsID, symbol, status string, ageMinutes int) string {
	t.Helper()
	var botID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, created_at
		) VALUES (
			$1, $2, $3, $4, 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $5, 'REAL', 'PENDING',
			NOW() - ($6::TEXT || ' minutes')::INTERVAL
		)
		RETURNING id
	`, accountID, settingsID, symbol, status,
		"itest-"+time.Now().Format("150405.000000000"), fmt.Sprintf("%d", ageMinutes)).Scan(&botID); err != nil {
		t.Fatalf("seed unknown submission %s: %v", symbol, err)
	}
	return botID
}

// TestStopPathsNeverTouchNullSubmissionRows is the CRIT-3(a) invariant: fleet
// stop must not stop-request rows whose exchange id is still unknown — that
// made unsupervisable STOP_REQUESTED zombies blocking the symbol. The rows
// stay with adoption, which then adopts them (PENDING_SUBMISSION → RUNNING;
// a pre-existing STOP_REQUESTED zombie keeps its stop intent for the manage
// loop to cancel) or clears provably-never-created rows to FAILED.
func TestStopPathsNeverTouchNullSubmissionRows(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const helperSymbol = "ADOPTH_USDT_PERP"
	mock := newAdoptionExchangeMock(t, helperSymbol, "100")
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-adoption-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-adoption-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-adoption-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-adoption-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $1)`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id IN (
			SELECT id::TEXT FROM grid_bots WHERE account_id = $1)`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM notification_outbox WHERE payload::TEXT LIKE '%ADOPT%'`)
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

	pendingID := seedUnknownSubmission(t, pool, account.ID, settings.ID, "ADOPTP_USDT_PERP", "PENDING_SUBMISSION", 5)
	zombieID := seedUnknownSubmission(t, pool, account.ID, settings.ID, "ADOPTZ_USDT_PERP", "STOP_REQUESTED", 5)
	staleID := seedUnknownSubmission(t, pool, account.ID, settings.ID, "ADOPTS_USDT_PERP", "STOP_REQUESTED", 40)

	// A RUNNING helper bot keeps the manage loop (and the reconciliation
	// behind it) alive while the rows under test have no remote id.
	_, err = pool.Exec(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id
		) VALUES (
			$1, $2, $3, 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $4, 'REAL', 'REMOTE_ID_PERSISTED', 'ADOPT-HELP'
		)
	`, account.ID, settings.ID, helperSymbol, "itest-"+time.Now().Format("150405.000000000"))
	if err != nil {
		t.Fatalf("seed helper: %v", err)
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)), slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	// The fleet stop must leave every NULL-bu row exactly as it is.
	if err := worker.stop(ctx); err != nil {
		t.Fatalf("fleet stop: %v", err)
	}
	for _, row := range []struct{ id, want string }{
		{pendingID, "PENDING_SUBMISSION"},
		{zombieID, "STOP_REQUESTED"},
		{staleID, "STOP_REQUESTED"},
	} {
		var status string
		var closed *string
		if err := pool.QueryRow(ctx, `
			SELECT status, closed_reason FROM grid_bots WHERE id = $1
		`, row.id).Scan(&status, &closed); err != nil {
			t.Fatalf("load row after stop: %v", err)
		}
		if status != row.want || closed != nil {
			t.Fatalf("stop must not touch NULL-bu rows: want %s/NULL, got %s/%v", row.want, status, closed)
		}
	}

	// The exchange holds grids for the pending row and the zombie; the stale
	// row (40 min, complete lists, no match) must be cleared as never created.
	createdMS := func(ageMinutes int) int64 {
		return time.Now().Add(-time.Duration(ageMinutes) * time.Minute).UnixMilli()
	}
	mock.setOrders([]map[string]any{
		{
			"buOrderId": "ADOPT-P", "buOrderType": "futures_grid", "status": "running",
			"base": "ADOPTP", "quote": "USDT", "createTime": createdMS(5),
			"buOrderData": map[string]any{"quoteInvestment": "100"},
		},
		{
			"buOrderId": "ADOPT-Z", "buOrderType": "futures_grid", "status": "running",
			"base": "ADOPTZ", "quote": "USDT", "createTime": createdMS(5),
			"buOrderData": map[string]any{"quoteInvestment": "100"},
		},
	})

	// Back to RUNNING so a stop pass does not skew the second round, then the
	// adoption manage pass.
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings SET status = 'RUNNING' WHERE scope_key = $1::VARCHAR
	`, DefaultScope); err != nil {
		t.Fatalf("restore autopilot: %v", err)
	}
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	var pendingStatus string
	var pendingBU *string
	if err := pool.QueryRow(ctx, `
		SELECT status, bu_order_id FROM grid_bots WHERE id = $1
	`, pendingID).Scan(&pendingStatus, &pendingBU); err != nil {
		t.Fatalf("load pending row: %v", err)
	}
	if pendingStatus != "RUNNING" || pendingBU == nil || *pendingBU != "ADOPT-P" {
		t.Fatalf("pending submission must be adopted as RUNNING with its buOrderId, got %s/%v", pendingStatus, pendingBU)
	}

	var zombieStatus string
	var zombieBU *string
	if err := pool.QueryRow(ctx, `
		SELECT status, bu_order_id FROM grid_bots WHERE id = $1
	`, zombieID).Scan(&zombieStatus, &zombieBU); err != nil {
		t.Fatalf("load zombie row: %v", err)
	}
	if zombieStatus != "STOP_REQUESTED" || zombieBU == nil || *zombieBU != "ADOPT-Z" {
		t.Fatalf("STOP_REQUESTED zombie must adopt its buOrderId while KEEPING the stop intent, got %s/%v", zombieStatus, zombieBU)
	}

	var staleStatus, staleReason string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason,'') FROM grid_bots WHERE id = $1
	`, staleID).Scan(&staleStatus, &staleReason); err != nil {
		t.Fatalf("load stale row: %v", err)
	}
	if staleStatus != "FAILED" || staleReason != "NOT_CREATED_ON_EXCHANGE" {
		t.Fatalf("provably-never-created zombie must be cleared to FAILED/NOT_CREATED_ON_EXCHANGE, got %s/%s", staleStatus, staleReason)
	}
}

// TestCreateRaceRecordsBuOrderId reproduces the CloseAll×deployReal race at
// its exact window: a stop request lands on the PENDING_SUBMISSION row while
// the create call is in flight. The lifecycle persist must still record the
// remote buOrderId (widened WHERE), preserve the stop intent (status guard)
// and carry the tranche markers from the INSERT itself.
func TestCreateRaceRecordsBuOrderId(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const symbol = "RACEX_USDT_PERP"
	var raceHits int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/common/symbols", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"symbols": []map[string]any{
				{"symbol": symbol, "type": "PERP", "enable": true, "status": "TRADING"},
			}},
		})
	})
	// The CloseAll stand-in: flip the just-inserted PENDING_SUBMISSION row to
	// STOP_REQUESTED inside the create window, exactly as the raced bulk
	// UPDATE would.
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/create", func(w http.ResponseWriter, _ *http.Request) {
		tag, err := pool.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'STOP_REQUESTED', closed_reason = 'AUTOGRID_STOP', updated_at = NOW()
			WHERE symbol = $1 AND status = 'PENDING_SUBMISSION'
		`, symbol)
		if err == nil {
			raceHits = int(tag.RowsAffected())
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"buOrderId": "RACE-1"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	accountName := "integration-race-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-race-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-race-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-race-test%'`)
	accountService := accounts.NewService(pool)
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

	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	var savedAccountID *string
	var savedStatus, savedMode string
	if err := pool.QueryRow(ctx, `
		SELECT account_id, status, execution_mode FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID, &savedStatus, &savedMode); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode)
	})
	_, _ = pool.Exec(ctx, `
		UPDATE autogrid_settings SET account_id = $2, status = 'RUNNING', execution_mode = 'REAL'
		WHERE scope_key = $1::VARCHAR
	`, DefaultScope, account.ID)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	manager := grid.NewLifecycleManager(pool, pionex.NewClient(server.URL, "itest-key", "itest-secret"))
	target := decimal.RequireFromString("4.5")
	maxLoss := decimal.RequireFromString("2")
	botID, createErr := manager.CreateGridBot(ctx, grid.CreateInput{
		AccountID:          account.ID,
		AutoGridSettingsID: &settings.ID,
		IdempotencyKey:     "race-" + time.Now().Format("150405.000000000"),
		Params: pionex.NativeFuturesGridCreateParams{
			Base: "RACEX", Quote: "USDT",
			BUOrderData: pionex.BUOrderData{
				Top:    decimal.RequireFromString("110"),
				Bottom: decimal.RequireFromString("90"),
				Row:    20, GridType: "arithmetic", Trend: "no_trend", Leverage: 2,
				QuoteInvestment: decimal.RequireFromString("100"),
			},
		},
		PnLTargetUSDT: &target,
		MaxLossUSDT:   &maxLoss,
		TrancheState: map[string]any{
			"trancheDeployed": 1,
			"trancheBase":     "100",
			"trancheEntry":    "100",
			"atrPctEntry":     0.5,
		},
	})
	if createErr != nil {
		t.Fatalf("raced create must still succeed and record the remote id, got: %v", createErr)
	}
	if raceHits != 1 {
		t.Fatalf("fixture sanity: the stop race must hit exactly the pending row, got %d", raceHits)
	}

	var status string
	var buOrderID *string
	var trancheBase string
	if err := pool.QueryRow(ctx, `
		SELECT status, bu_order_id, COALESCE(model_state->>'trancheBase','')
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&status, &buOrderID, &trancheBase); err != nil {
		t.Fatalf("load raced bot: %v", err)
	}
	if buOrderID == nil || *buOrderID != "RACE-1" {
		t.Fatalf("the raced bot must carry its exchange buOrderId (CRIT-3c), got %v", buOrderID)
	}
	if status != "STOP_REQUESTED" {
		t.Fatalf("the raced stop intent must survive the lifecycle persist, got %s", status)
	}
	if trancheBase != "100" {
		t.Fatalf("tranche markers must ride the INSERT (v2.0.78), got trancheBase=%q", trancheBase)
	}
	if strings.TrimSpace(botID) == "" {
		t.Fatal("create must return the grid id")
	}
}
