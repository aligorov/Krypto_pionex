package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
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

// trancheSelfHealMock is the tranche2ExchangeMock shape plus the exchange
// investment the v2.0.78 self-heal compares against: the detail endpoint
// reports quoteInvestment, and a successful invest_in moves it to the full
// tranche exactly like the real exchange would.
type trancheSelfHealMock struct {
	server      *httptest.Server
	mu          sync.Mutex
	investment  string
	investCalls int
	lastBody    map[string]any
}

func (m *trancheSelfHealMock) setInvestment(value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.investment = value
}

func (m *trancheSelfHealMock) investCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.investCalls
}

func newTrancheSelfHealMock(t *testing.T, symbol, price, investment string) *trancheSelfHealMock {
	t.Helper()
	mock := &trancheSelfHealMock{investment: investment}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		investment := mock.investment
		mock.mu.Unlock()
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
					"quoteInvestment": investment,
					"position":        "0", "positionOpenPrice": price,
					"profitReduce": "0.1", "profitWithdrawn": "0",
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
				{"symbol": symbol, "indexPrice": price, "markPrice": price},
			}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": []map[string]any{
				{"symbol": symbol, "close": price, "open": price,
					"high": price, "low": price, "volume": "1000"},
			}},
		})
	})
	// Too few candles → regimeForSymbol returns "" → not trending → the 24h
	// time-box top-up path is reachable.
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": []map[string]any{
				{"time": time.Now().UnixMilli(), "open": price, "close": price,
					"high": price, "low": price, "volume": "1"},
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
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParams", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.mu.Lock()
		defer mock.mu.Unlock()
		mock.investCalls++
		mock.lastBody = body
		if body["type"] == "invest_in" {
			// The exchange accepted the margin: the grid now holds the full
			// tranche regardless of what our local persist does next.
			// decimal.Decimal marshals unquoted, so tolerate float64/string.
			var added decimal.Decimal
			switch quote := body["quoteInvestment"].(type) {
			case string:
				added, _ = decimal.NewFromString(quote)
			case float64:
				added = decimal.NewFromFloat(quote)
			}
			if current, curErr := decimal.NewFromString(mock.investment); curErr == nil {
				mock.investment = current.Add(added).String()
			}
		}
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

// trancheSelfHealFixture is the shared stand: dedicated account on the mock
// exchange, pinned risk caps, RUNNING/REAL settings. Returns the worker and
// cleanup handles.
func trancheSelfHealFixture(t *testing.T, mockServerURL string) (*Worker, *pgxpool.Pool, string) {
	t.Helper()
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

	accountName := "integration-trancheheal-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-trancheheal-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-trancheheal-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-trancheheal-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-trancheheal-test%'`)
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
		_, _ = pool.Exec(ctx, `DELETE FROM notification_outbox WHERE event_type IN ('TRANCHE_2','TRANCHE_RESYNC')`)
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})
	mockClient := pionex.NewClient(mockServerURL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	var savedSymbolCap, savedAccountCap, savedDaily string
	if err := pool.QueryRow(ctx, `
		SELECT max_symbol_exposure_usd::TEXT, max_account_exposure_usd::TEXT, max_daily_loss_usd::TEXT
		FROM risk_settings WHERE id = 1
	`).Scan(&savedSymbolCap, &savedAccountCap, &savedDaily); err != nil {
		t.Fatalf("snapshot risk caps: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE risk_settings
			SET max_symbol_exposure_usd = $2::NUMERIC, max_account_exposure_usd = $3::NUMERIC,
			    max_daily_loss_usd = $4::NUMERIC
			WHERE id = 1
		`, savedSymbolCap, savedAccountCap, savedDaily)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = false,
		    max_symbol_exposure_usd = 100000, max_account_exposure_usd = 100000,
		    max_daily_loss_usd = 100000, max_leverage = 10
		WHERE id = 1
	`); err != nil {
		t.Fatalf("pin risk caps: %v", err)
	}

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
	worker.publicClient = pionex.NewClient(mockServerURL, "", "")
	return worker, pool, settings.ID
}

// seedTrancheBot lands one RUNNING REAL tranche-1 bot. initialState is merged
// into model_state on top of the tranche markers.
func seedTrancheBot(t *testing.T, pool *pgxpool.Pool, accountID, settingsID, symbol, buOrderID string, investment decimal.Decimal, initialState map[string]any) string {
	t.Helper()
	modelState := map[string]any{
		"trancheDeployed": 1,
		"trancheBase":     "200",
		"trancheEntry":    "100",
		"atrPctEntry":     0.5,
	}
	for key, value := range initialState {
		modelState[key] = value
	}
	var botID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			pnl_target_usdt, max_loss_usdt, created_at, bot_number, model_state
		) VALUES (
			$1, $2, $3, 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			$4, 0, $5, 'REAL', 'REMOTE_ID_PERSISTED', $6,
			4.5, 2, NOW() - INTERVAL '25 hours', 778, $7::JSONB
		)
		RETURNING id
	`, accountID, settingsID, symbol, investment,
		"itest-"+time.Now().Format("150405.000000000"), buOrderID, modelState).Scan(&botID); err != nil {
		t.Fatalf("seed tranche bot: %v", err)
	}
	return botID
}

type trancheBotState struct {
	trancheDeployed int
	investment      decimal.Decimal
	target          decimal.Decimal
	maxLoss         decimal.Decimal
	intentMarker    *string
}

func loadTrancheBotState(t *testing.T, pool *pgxpool.Pool, botID string) trancheBotState {
	t.Helper()
	var state trancheBotState
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       quote_investment, pnl_target_usdt, max_loss_usdt,
		       model_state->>'trancheIntentAt'
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&state.trancheDeployed, &state.investment, &state.target,
		&state.maxLoss, &state.intentMarker); err != nil {
		t.Fatalf("load tranche bot %s: %v", botID, err)
	}
	return state
}

// blockBotUpdates installs a BEFORE UPDATE trigger on grid_bots that raises
// only for rows matching the predicate (OLD.<col> IS DISTINCT FROM NEW.<col>
// for the given bot), simulating a persist failure at an exact write site.
func blockBotUpdates(t *testing.T, pool *pgxpool.Pool, botID, column string) {
	t.Helper()
	// pgx's extended protocol forbids multi-statement Execs — one call per DDL.
	if _, err := pool.Exec(context.Background(), `
		CREATE OR REPLACE FUNCTION itest_tranche_block_persist() RETURNS trigger AS $fn$
		BEGIN
			RAISE EXCEPTION 'itest: simulated persist failure';
		END $fn$ LANGUAGE plpgsql
	`); err != nil {
		t.Fatalf("install persist blocker function: %v", err)
	}
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		CREATE TRIGGER itest_tranche_block_persist
		BEFORE UPDATE ON grid_bots
		FOR EACH ROW
		WHEN (OLD.id = '%s'::UUID AND NEW.%s IS DISTINCT FROM OLD.%s)
		EXECUTE FUNCTION itest_tranche_block_persist()
	`, botID, column, column)); err != nil {
		t.Fatalf("install %s persist blocker: %v", column, err)
	}
	t.Cleanup(func() { dropPersistBlocker(t, pool) })
}

func dropPersistBlocker(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS itest_tranche_block_persist ON grid_bots`); err != nil {
		t.Fatalf("drop persist blocker trigger: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS itest_tranche_block_persist()`); err != nil {
		t.Fatalf("drop persist blocker function: %v", err)
	}
}

func trancheResyncActions(t *testing.T, pool *pgxpool.Pool, botID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT details->>'action' FROM bot_execution_events
		WHERE bot_id = $1::TEXT AND event_type = 'TRANCHE_RESYNC'
		ORDER BY created_at
	`, botID)
	if err != nil {
		t.Fatalf("load TRANCHE_RESYNC events: %v", err)
	}
	defer rows.Close()
	actions := make([]string, 0, 2)
	for rows.Next() {
		var action *string
		if err := rows.Scan(&action); err == nil && action != nil {
			actions = append(actions, *action)
		}
	}
	return actions
}

// TestTrancheSelfHealResyncNoSecondPour is the CRIT-2 core invariant: a
// previous invest_in that landed on the exchange without its local persist
// (local 100 vs remote 200, fresh intent marker) must NEVER be poured again —
// the manage pass resyncs the local column to exchange truth and completes
// the target/stop doubling instead.
func TestTrancheSelfHealResyncNoSecondPour(t *testing.T) {
	const symbol = "TRHEAL_A_USDT_PERP"
	mock := newTrancheSelfHealMock(t, symbol, "100", "200")
	worker, pool, settingsID := trancheSelfHealFixture(t, mock.server.URL)
	ctx := context.Background()

	var accountID string
	if err := pool.QueryRow(ctx, `
		SELECT account_id FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&accountID); err != nil {
		t.Fatalf("resolve fixture account: %v", err)
	}

	freshIntent := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	botID := seedTrancheBot(t, pool, accountID, settingsID, symbol, "TRHEAL-A",
		decimal.NewFromInt(100), map[string]any{"trancheIntentAt": freshIntent})

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	if got := mock.investCount(); got != 0 {
		t.Fatalf("a landed tranche must never be re-poured, got %d invest_in calls", got)
	}
	state := loadTrancheBotState(t, pool, botID)
	if !state.investment.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("local investment must resync to the exchange truth 200, got %s", state.investment)
	}
	if state.trancheDeployed != 2 ||
		!state.target.Equal(decimal.NewFromInt(9)) ||
		!state.maxLoss.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("self-heal must complete the doubling: tranche=%d target=%s loss=%s",
			state.trancheDeployed, state.target, state.maxLoss)
	}
	actions := trancheResyncActions(t, pool, botID)
	if len(actions) != 2 || actions[0] != "investment_resynced" || actions[1] != "targets_doubled" {
		t.Fatalf("expected resync+doubling TRANCHE_RESYNC events, got %v", actions)
	}
}

// TestTrancheSelfHealCompletesFailedDoubling: the pour and its persist both
// land but the target-doubling UPDATE falls over — the next manage pass must
// complete the doubling idempotently without a second invest_in.
func TestTrancheSelfHealCompletesFailedDoubling(t *testing.T) {
	const symbol = "TRHEAL_B_USDT_PERP"
	mock := newTrancheSelfHealMock(t, symbol, "100", "100")
	worker, pool, settingsID := trancheSelfHealFixture(t, mock.server.URL)
	ctx := context.Background()

	var accountID string
	if err := pool.QueryRow(ctx, `
		SELECT account_id FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&accountID); err != nil {
		t.Fatalf("resolve fixture account: %v", err)
	}

	botID := seedTrancheBot(t, pool, accountID, settingsID, symbol, "TRHEAL-B",
		decimal.NewFromInt(100), nil)
	blockBotUpdates(t, pool, botID, "pnl_target_usdt")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}
	if got := mock.investCount(); got != 1 {
		t.Fatalf("pass 1 must pour exactly once, got %d", got)
	}
	mid := loadTrancheBotState(t, pool, botID)
	if mid.trancheDeployed != 1 {
		t.Fatalf("blocked doubling must leave trancheDeployed=1, got %d", mid.trancheDeployed)
	}
	if !mid.investment.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("invest persist must have landed (local 200), got %s", mid.investment)
	}

	dropPersistBlocker(t, pool)
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	if got := mock.investCount(); got != 1 {
		t.Fatalf("self-heal must not re-pour a committed tranche, got %d calls", got)
	}
	state := loadTrancheBotState(t, pool, botID)
	if state.trancheDeployed != 2 ||
		!state.target.Equal(decimal.NewFromInt(9)) ||
		!state.maxLoss.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("self-heal must finish the doubling: tranche=%d target=%s loss=%s",
			state.trancheDeployed, state.target, state.maxLoss)
	}
	if actions := trancheResyncActions(t, pool, botID); len(actions) != 1 || actions[0] != "targets_doubled" {
		t.Fatalf("exactly one targets_doubled event expected, got %v", actions)
	}
}

// TestTrancheSelfHealAfterPersistFailure: the native invest_in succeeds but
// its local persist falls over (the double-pour trap of CRIT-2). After the
// backoff expires, the retry consults the exchange first: remote already
// holds $200, so no second pour — local resyncs and the doubling completes.
func TestTrancheSelfHealAfterPersistFailure(t *testing.T) {
	const symbol = "TRHEAL_C_USDT_PERP"
	mock := newTrancheSelfHealMock(t, symbol, "100", "100")
	worker, pool, settingsID := trancheSelfHealFixture(t, mock.server.URL)
	ctx := context.Background()

	var accountID string
	if err := pool.QueryRow(ctx, `
		SELECT account_id FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&accountID); err != nil {
		t.Fatalf("resolve fixture account: %v", err)
	}

	botID := seedTrancheBot(t, pool, accountID, settingsID, symbol, "TRHEAL-C",
		decimal.NewFromInt(100), nil)
	// AdjustBot's invest persist increments adjustments_count — block exactly
	// that write so the native call succeeds and the persist fails.
	blockBotUpdates(t, pool, botID, "adjustments_count")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}
	if got := mock.investCount(); got != 1 {
		t.Fatalf("pass 1 must reach the exchange exactly once, got %d", got)
	}
	after := loadTrancheBotState(t, pool, botID)
	if after.investment.Equal(decimal.NewFromInt(200)) {
		t.Fatal("fixture sanity: the persist blocker must keep the local investment at 100")
	}
	if after.intentMarker == nil {
		t.Fatal("the durable intent marker must survive a persist failure (the pour may have landed)")
	}

	// Expire the 1h fail backoff and remove the blocker: the retry window opens.
	dropPersistBlocker(t, pool)
	if _, err := pool.Exec(ctx, `
		UPDATE grid_bots
		SET model_state = jsonb_set(model_state, '{trancheFailAt}',
			to_jsonb(to_char(NOW() AT TIME ZONE 'UTC' - INTERVAL '2 hours', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))
		WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("expire backoff: %v", err)
	}

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	if got := mock.investCount(); got != 1 {
		t.Fatalf("the retry must NOT pour a second time — the exchange already holds the tranche, got %d calls", got)
	}
	state := loadTrancheBotState(t, pool, botID)
	if !state.investment.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("local investment must resync to exchange 200, got %s", state.investment)
	}
	if state.trancheDeployed != 2 ||
		!state.target.Equal(decimal.NewFromInt(9)) ||
		!state.maxLoss.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("self-heal must complete the doubling: tranche=%d target=%s loss=%s",
			state.trancheDeployed, state.target, state.maxLoss)
	}
	actions := trancheResyncActions(t, pool, botID)
	if len(actions) != 2 || actions[0] != "investment_resynced" || actions[1] != "targets_doubled" {
		t.Fatalf("expected resync+doubling TRANCHE_RESYNC events, got %v", actions)
	}
}
