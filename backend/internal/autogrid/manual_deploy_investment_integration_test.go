package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// manualDeployExchangeMock serves every endpoint DeployManualBot touches:
// public tickers/klines (the service's publicAPI is pointed at it), the
// common symbols list CreateGridBot validates the symbol against, and the
// signed futuresGrid checkParams/create calls. minInvestment is switchable
// per round; every create body's quoteInvestment is captured.
type manualDeployExchangeMock struct {
	mu            sync.Mutex
	minInvestment string
	checkCalls    int
	createCalls   int
	createBodies  []map[string]any
	server        *httptest.Server
}

func newManualDeployExchangeMock(t *testing.T, minInvestment string) *manualDeployExchangeMock {
	t.Helper()
	mock := &manualDeployExchangeMock{minInvestment: minInvestment}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, payload any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": r.URL.Query().Get("symbol"), "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	})
	// 60 deterministic hourly candles oscillating around 100: sigma and ATR
	// are identical for every deploy, so DYNAMIC targets differ ONLY by budget.
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, r *http.Request) {
		klines := make([]map[string]any, 0, 60)
		for i := 0; i < 60; i++ {
			close := "100.5"
			if i%2 == 0 {
				close = "99.5"
			}
			klines = append(klines, map[string]any{
				"time": time.Now().UnixMilli() - int64(60-i)*3600_000,
				"open": "100", "close": close, "high": "101", "low": "99", "volume": "10",
			})
		}
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": klines},
		})
	})
	mux.HandleFunc("GET /api/v1/common/symbols", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"symbols": []map[string]any{
					{"symbol": "CANREF_USDT_PERP", "type": "PERP", "enable": true, "status": "TRADING"},
					{"symbol": "CANARY_USDT_PERP", "type": "PERP", "enable": true, "status": "TRADING"},
					{"symbol": "CANRA_USDT_PERP", "type": "PERP", "enable": true, "status": "TRADING"},
					{"symbol": "CANRB_USDT_PERP", "type": "PERP", "enable": true, "status": "TRADING"},
					{"symbol": "CANRC_USDT_PERP", "type": "PERP", "enable": true, "status": "TRADING"},
					{"symbol": "CANE1_USDT_PERP", "type": "PERP", "enable": true, "status": "TRADING"},
				},
			},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/checkParams", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.checkCalls++
		min := mock.minInvestment
		mock.mu.Unlock()
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"minInvestment": min},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/create", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if nested, ok := body["buOrderData"].(map[string]any); ok {
			body = nested
		}
		mock.mu.Lock()
		mock.createCalls++
		mock.createBodies = append(mock.createBodies, body)
		calls := mock.createCalls
		mock.mu.Unlock()
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"buOrderId": fmt.Sprintf("MANUAL-CREATE-%d", calls)},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *manualDeployExchangeMock) setMinInvestment(min string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.minInvestment = min
}

func (m *manualDeployExchangeMock) creates() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalls
}

func (m *manualDeployExchangeMock) lastCreateInvestment() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.createBodies) == 0 {
		return ""
	}
	body := m.createBodies[len(m.createBodies)-1]
	value, _ := body["quoteInvestment"].(string)
	return value
}

// snapshotManualSettings pins the mutable autogrid_settings / risk_settings /
// REAL execution gate rows the tests touch and restores them on cleanup.
type manualSettingsSnapshot struct {
	accountID       *string
	status          string
	executionMode   string
	budget          string
	pnlTargetMode   string
	pnlTargetUSDT   string
	maxLossUSDT     string
	killSwitch      bool
	maxDailyLoss    string
	maxAccountExp   string
	maxSymbolExp    string
	maxLeverage     int
	maxActiveGrids  int
	configRealGrid  string
	flagRealGrid    bool
	flagRealEnabled bool
}

func snapshotManualSettings(t *testing.T, pool *pgxpool.Pool, settingsID string) *manualSettingsSnapshot {
	t.Helper()
	ctx := context.Background()
	snap := &manualSettingsSnapshot{}
	if err := pool.QueryRow(ctx, `
		SELECT account_id, status, execution_mode, budget_usdt::TEXT,
		       pnl_target_mode, pnl_target_usdt::TEXT, max_loss_usdt::TEXT
		FROM autogrid_settings WHERE id = $1
	`, settingsID).Scan(&snap.accountID, &snap.status, &snap.executionMode, &snap.budget,
		&snap.pnlTargetMode, &snap.pnlTargetUSDT, &snap.maxLossUSDT); err != nil {
		t.Fatalf("snapshot autogrid_settings: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT kill_switch_enabled, max_daily_loss_usd::TEXT, max_account_exposure_usd::TEXT,
		       max_symbol_exposure_usd::TEXT, max_leverage, max_active_grid_bots
		FROM risk_settings WHERE id = 1
	`).Scan(&snap.killSwitch, &snap.maxDailyLoss, &snap.maxAccountExp,
		&snap.maxSymbolExp, &snap.maxLeverage, &snap.maxActiveGrids); err != nil {
		t.Fatalf("snapshot risk_settings: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(value #>> '{}', 'false') FROM app_config
		WHERE key = 'real_grid_execution_enabled'
	`).Scan(&snap.configRealGrid); err != nil {
		t.Fatalf("snapshot app_config: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT enabled FROM feature_flags WHERE name = 'real_native_grid'
	`).Scan(&snap.flagRealGrid); err != nil {
		t.Fatalf("snapshot feature_flags: %v", err)
	}
	snap.flagRealEnabled = snap.flagRealGrid
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4, budget_usdt = $5::NUMERIC,
			    pnl_target_mode = $6, pnl_target_usdt = $7::NUMERIC, max_loss_usdt = $8::NUMERIC
			WHERE id = $1
		`, settingsID, snap.accountID, snap.status, snap.executionMode, snap.budget,
			snap.pnlTargetMode, snap.pnlTargetUSDT, snap.maxLossUSDT); err != nil {
			t.Errorf("restore autogrid_settings: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE risk_settings
			SET kill_switch_enabled = $1, max_daily_loss_usd = $2::NUMERIC,
			    max_account_exposure_usd = $3::NUMERIC, max_symbol_exposure_usd = $4::NUMERIC,
			    max_leverage = $5, max_active_grid_bots = $6
			WHERE id = 1
		`, snap.killSwitch, snap.maxDailyLoss, snap.maxAccountExp, snap.maxSymbolExp,
			snap.maxLeverage, snap.maxActiveGrids); err != nil {
			t.Errorf("restore risk_settings: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE app_config SET value = $1::JSONB, updated_at = NOW()
			WHERE key = 'real_grid_execution_enabled'
		`, snap.configRealGrid); err != nil {
			t.Errorf("restore app_config: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE feature_flags SET enabled = $1, updated_at = NOW()
			WHERE name = 'real_native_grid'
		`, snap.flagRealEnabled); err != nil {
			t.Errorf("restore feature_flags: %v", err)
		}
	})
	return snap
}

// TestManualDeployPaperInvestmentOverride pins the canary math on the PAPER
// path: an investment override drives quote_investment AND the derived
// DYNAMIC target/stop (exactly budget-proportional), while the floor/budget
// guards refuse typos and oversized canaries before anything is written.
func TestManualDeployPaperInvestmentOverride(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	service := NewService(pool, risk.NewEngine(pool))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	snapshotManualSettings(t, pool, settings.ID)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'CAN%_USDT_PERP'`); err != nil {
			t.Errorf("cleanup paper bots: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET budget_usdt = 500, pnl_target_mode = 'DYNAMIC',
		    pnl_target_usdt = 0, max_loss_usdt = 0
		WHERE id = $1
	`, settings.ID); err != nil {
		t.Fatalf("pin settings: %v", err)
	}

	mock := newManualDeployExchangeMock(t, "30")
	service.publicAPI = pionex.NewClient(mock.server.URL, "", "")

	deploy := func(symbol string, investment decimal.Decimal) (*ActiveBot, error) {
		t.Helper()
		input := ManualDeployInput{
			Symbol: symbol, Mode: "PAPER", Direction: "NEUTRAL",
			Leverage: 2, Lower: decimal.NewFromInt(90),
			Upper: decimal.NewFromInt(110), Row: 10,
		}
		if investment.IsPositive() {
			input.Investment = investment
		}
		bot, _, deployErr := service.DeployManualBot(ctx, nil, input)
		return bot, deployErr
	}
	paperRow := func(t *testing.T, symbol string) (string, string, string) {
		t.Helper()
		var investment, target, loss string
		if err := pool.QueryRow(ctx, `
			SELECT quote_investment::TEXT, pnl_target_usdt::TEXT, max_loss_usdt::TEXT
			FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
		`, symbol).Scan(&investment, &target, &loss); err != nil {
			t.Fatalf("load paper bot %s: %v", symbol, err)
		}
		return investment, target, loss
	}

	// Reference: no override — everything inherits the $500 fleet budget.
	refBot, err := deploy("CANREF_USDT_PERP", decimal.Zero)
	if err != nil {
		t.Fatalf("reference deploy: %v", err)
	}
	if !refBot.QuoteInvestment.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("reference bot must carry the fleet budget 500, got %s", refBot.QuoteInvestment)
	}
	refInvestment, refTarget, refLoss := paperRow(t, "CANREF_USDT_PERP")
	if refInvestment != "500.00000000" && refInvestment != "500" {
		t.Fatalf("reference quote_investment must be 500, got %s", refInvestment)
	}

	// Canary: $50 override — investment, target and stop all follow it.
	canaryBot, err := deploy("CANARY_USDT_PERP", decimal.NewFromInt(50))
	if err != nil {
		t.Fatalf("canary deploy: %v", err)
	}
	if !canaryBot.QuoteInvestment.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("canary bot must report QuoteInvestment 50, got %s", canaryBot.QuoteInvestment)
	}
	canaryInvestment, canaryTarget, canaryLoss := paperRow(t, "CANARY_USDT_PERP")
	if canaryInvestment != "50.00000000" && canaryInvestment != "50" {
		t.Fatalf("canary quote_investment must be 50, got %s", canaryInvestment)
	}
	refTargetNum, _ := decimal.NewFromString(refTarget)
	refLossNum, _ := decimal.NewFromString(refLoss)
	canaryTargetNum, _ := decimal.NewFromString(canaryTarget)
	canaryLossNum, _ := decimal.NewFromString(canaryLoss)
	if canaryTargetNum.Equal(decimal.Zero) || canaryLossNum.Equal(decimal.Zero) {
		t.Fatalf("DYNAMIC mode must derive nonzero targets, got %s / %s", canaryTarget, canaryLoss)
	}
	// Same candles → identical percentages → the canary's amounts are exactly
	// one tenth of the $500 reference (float tolerance for the last ulp).
	scaledTarget := canaryTargetNum.Mul(decimal.NewFromInt(10)).InexactFloat64()
	if diff := scaledTarget - refTargetNum.InexactFloat64(); diff < -1e-6 || diff > 1e-6 {
		t.Fatalf("canary target %s × 10 must equal reference %s (diff %v)", canaryTarget, refTarget, diff)
	}
	scaledLoss := canaryLossNum.Mul(decimal.NewFromInt(10)).InexactFloat64()
	if diff := scaledLoss - refLossNum.InexactFloat64(); diff < -1e-6 || diff > 1e-6 {
		t.Fatalf("canary stop %s × 10 must equal reference %s (diff %v)", canaryLoss, refLoss, diff)
	}
	if !canaryLossNum.LessThan(refLossNum) || !canaryTargetNum.LessThan(refTargetNum) {
		t.Fatalf("canary amounts must shrink with the margin: %s/%s vs %s/%s",
			canaryTarget, canaryLoss, refTarget, refLoss)
	}

	// Floor guard: $1 is a typo, not a canary — refused, nothing written.
	if _, err := deploy("CANLOW_USDT_PERP", decimal.NewFromInt(1)); err == nil ||
		!strings.Contains(err.Error(), "$5") {
		t.Fatalf("override $1 must be refused with the $5 floor, got %v", err)
	}
	var lowCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = 'CANLOW_USDT_PERP'
	`).Scan(&lowCount); err != nil || lowCount != 0 {
		t.Fatalf("refused canary must not leave a row, got %d (%v)", lowCount, err)
	}

	// Budget guard: a canary bigger than the fleet is a fat finger.
	if _, err := deploy("CANBIG_USDT_PERP", decimal.NewFromInt(600)); err == nil ||
		!strings.Contains(err.Error(), "exceeds the fleet budget") {
		t.Fatalf("override 600 > budget 500 must be refused, got %v", err)
	}
	var bigCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = 'CANBIG_USDT_PERP'
	`).Scan(&bigCount); err != nil || bigCount != 0 {
		t.Fatalf("oversized canary must not leave a row, got %d (%v)", bigCount, err)
	}
}

// TestManualDeployRealInvestmentOverride pins the REAL canary path end to end
// against a mocked exchange: the override flows into risk.ValidateNewGrid,
// the native QuoteInvestment payload and the min-investment comparison, and
// the no-override deploy keeps inheriting the fleet budget.
func TestManualDeployRealInvestmentOverride(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	accountService := accounts.NewService(pool)
	service := NewService(pool, risk.NewEngine(pool))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	snapshotManualSettings(t, pool, settings.ID)

	accountName := "integration-manual-deploy-" + time.Now().Format("150405.000000000")
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		// Detach the settings pointer BEFORE deleting the account: the FK on
		// autogrid_settings.account_id would otherwise reject the delete (the
		// snapshot restore runs later, in LIFO order).
		if _, err := pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("detach settings account: %v", err)
		}
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

	mock := newManualDeployExchangeMock(t, "30")
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()
	service.publicAPI = pionex.NewClient(mock.server.URL, "", "")

	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, budget_usdt = 500, pnl_target_mode = 'STATIC',
		    pnl_target_usdt = 6, max_loss_usdt = 8
		WHERE id = $1
	`, settings.ID, account.ID); err != nil {
		t.Fatalf("pin settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = false, max_daily_loss_usd = 10000,
		    max_account_exposure_usd = 100000, max_symbol_exposure_usd = 100000,
		    max_leverage = 10, max_active_grid_bots = 10
		WHERE id = 1
	`); err != nil {
		t.Fatalf("pin risk settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE app_config SET value = 'true'::JSONB, updated_at = NOW()
		WHERE key = 'real_grid_execution_enabled'
	`); err != nil {
		t.Fatalf("enable real grid execution: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE feature_flags SET enabled = true, updated_at = NOW()
		WHERE name = 'real_native_grid'
	`); err != nil {
		t.Fatalf("enable real native grid flag: %v", err)
	}

	deploy := func(symbol string, investment decimal.Decimal) (*ActiveBot, error) {
		t.Helper()
		input := ManualDeployInput{
			Symbol: symbol, Mode: "REAL", Direction: "NEUTRAL",
			Leverage: 2, Lower: decimal.NewFromInt(90),
			Upper: decimal.NewFromInt(110), Row: 10,
		}
		if investment.IsPositive() {
			input.Investment = investment
		}
		bot, _, deployErr := service.DeployManualBot(ctx, accountService, input)
		return bot, deployErr
	}
	realRow := func(t *testing.T, symbol string) (string, string, string) {
		t.Helper()
		var investment, status, buOrderID string
		if err := pool.QueryRow(ctx, `
			SELECT quote_investment::TEXT, status, COALESCE(bu_order_id, '')
			FROM grid_bots WHERE symbol = $1 AND account_id = $2
		`, symbol, account.ID).Scan(&investment, &status, &buOrderID); err != nil {
			t.Fatalf("load grid bot %s: %v", symbol, err)
		}
		return investment, status, buOrderID
	}

	// Round 1: $50 canary against a $30 exchange minimum — passes, and the
	// native create payload carries exactly 50.
	bot, err := deploy("CANRA_USDT_PERP", decimal.NewFromInt(50))
	if err != nil {
		t.Fatalf("canary REAL deploy: %v", err)
	}
	if !bot.QuoteInvestment.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("canary REAL bot must report QuoteInvestment 50, got %s", bot.QuoteInvestment)
	}
	investment, status, buOrderID := realRow(t, "CANRA_USDT_PERP")
	if investment != "50.00000000" && investment != "50" {
		t.Fatalf("canary REAL quote_investment must be 50, got %s", investment)
	}
	if status != "RUNNING" || buOrderID == "" {
		t.Fatalf("canary REAL bot must be RUNNING with a persisted buOrderId, got %s / %q", status, buOrderID)
	}
	if got := mock.lastCreateInvestment(); got != "50" {
		t.Fatalf("native create payload must carry quoteInvestment 50, got %q", got)
	}
	// v2.0.89 round-trip fee ledger: the deploy books the taker entry fee on
	// the opened notional (0.05% × 50 × lev 2 = 0.05) with the fee breakdown
	// stamped in model_state.
	var canaryFees, canaryEntryFeeMarker string
	if err := pool.QueryRow(ctx, `
		SELECT fees_paid_usdt::TEXT, COALESCE(model_state->>'entryFeeUsdt','')
		FROM grid_bots WHERE symbol = 'CANRA_USDT_PERP' AND account_id = $1
	`, account.ID).Scan(&canaryFees, &canaryEntryFeeMarker); err != nil {
		t.Fatalf("load canary fee ledger: %v", err)
	}
	if canaryFees != "0.05000000" && canaryFees != "0.05" {
		t.Fatalf("canary deploy fee must be 0.05 (0.05%% x 50 x 2), got %s", canaryFees)
	}
	if canaryEntryFeeMarker != "0.05" {
		t.Fatalf("entry fee breakdown must ride in model_state.entryFeeUsdt, got %q", canaryEntryFeeMarker)
	}

	// Round 2: minimum rises to $60 — the comparison must use the canary's 50,
	// not the fleet's 500 (a 500-basis comparison would wrongly pass).
	mock.setMinInvestment("60")
	if _, err := deploy("CANRB_USDT_PERP", decimal.NewFromInt(50)); err == nil ||
		!strings.Contains(err.Error(), "50") || !strings.Contains(err.Error(), "60") {
		t.Fatalf("canary below the exchange minimum must be refused naming both amounts, got %v", err)
	}
	if got := mock.creates(); got != 1 {
		t.Fatalf("refused canary must not reach the exchange create, got %d creates", got)
	}
	var refusedCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE symbol = 'CANRB_USDT_PERP'
	`).Scan(&refusedCount); err != nil || refusedCount != 0 {
		t.Fatalf("refused canary must leave no grid row, got %d (%v)", refusedCount, err)
	}

	// Round 3: no override — full inheritance of the fleet budget (regression).
	mock.setMinInvestment("30")
	bot, err = deploy("CANRC_USDT_PERP", decimal.Zero)
	if err != nil {
		t.Fatalf("budget REAL deploy: %v", err)
	}
	if !bot.QuoteInvestment.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("no-override REAL bot must carry the fleet budget 500, got %s", bot.QuoteInvestment)
	}
	investment, _, _ = realRow(t, "CANRC_USDT_PERP")
	if investment != "500.00000000" && investment != "500" {
		t.Fatalf("no-override quote_investment must be 500, got %s", investment)
	}
	if got := mock.lastCreateInvestment(); got != "500" {
		t.Fatalf("no-override native create must carry quoteInvestment 500, got %q", got)
	}
}

// TestManualDeployRealStopEnvelopeGate closes the v2.0.71 parity hole: a
// manual REAL deploy now runs the same fleet stop-envelope gate as the scan
// deploys (joint paper+REAL sum + the candidate's FULL stop vs 0.8× the
// daily-loss breaker), and a manual bot stores its full stop — no tranche
// halving, so the reserve is exactly the computed botMaxLoss.
func TestManualDeployRealStopEnvelopeGate(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	accountService := accounts.NewService(pool)
	service := NewService(pool, risk.NewEngine(pool))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	snapshotManualSettings(t, pool, settings.ID)

	accountName := "integration-manual-envelope-" + time.Now().Format("150405.000000000")
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		// Detach the settings pointer BEFORE deleting the account (FK).
		if _, err := pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("detach settings account: %v", err)
		}
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
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'CANEF%_USDT_PERP'`); err != nil {
			t.Errorf("cleanup filler paper bots: %v", err)
		}
	})

	mock := newManualDeployExchangeMock(t, "30")
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()
	service.publicAPI = pionex.NewClient(mock.server.URL, "", "")

	// Breaker 50 → envelope ceiling 0.8×50 = 40. Fillers Σ stored stops = 36;
	// the manual candidate's STATIC full stop is 8 → 36 + 8 = 44 > 40.
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, budget_usdt = 500, pnl_target_mode = 'STATIC',
		    pnl_target_usdt = 6, max_loss_usdt = 8
		WHERE id = $1
	`, settings.ID, account.ID); err != nil {
		t.Fatalf("pin settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = false, max_daily_loss_usd = 50,
		    max_account_exposure_usd = 100000, max_symbol_exposure_usd = 100000,
		    max_leverage = 10, max_active_grid_bots = 10
		WHERE id = 1
	`); err != nil {
		t.Fatalf("pin risk settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE app_config SET value = 'true'::JSONB, updated_at = NOW()
		WHERE key = 'real_grid_execution_enabled'
	`); err != nil {
		t.Fatalf("enable real grid execution: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE feature_flags SET enabled = true, updated_at = NOW()
		WHERE name = 'real_native_grid'
	`); err != nil {
		t.Fatalf("enable real native grid flag: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, max_loss_usdt
			) VALUES (
				$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
				90, 110, 10, 2, 200,
				100, 100, 9
			)
		`, settings.ID, fmt.Sprintf("CANEF%d_USDT_PERP", i)); err != nil {
			t.Fatalf("insert filler bot %d: %v", i, err)
		}
	}

	deploy := func(investment decimal.Decimal) error {
		t.Helper()
		input := ManualDeployInput{
			Symbol: "CANE1_USDT_PERP", Mode: "REAL", Direction: "NEUTRAL",
			Leverage: 2, Lower: decimal.NewFromInt(90),
			Upper: decimal.NewFromInt(110), Row: 10,
		}
		if investment.IsPositive() {
			input.Investment = investment
		}
		_, _, deployErr := service.DeployManualBot(ctx, accountService, input)
		return deployErr
	}

	// Round 1: fleet Σ 36 + full stop 8 = 44 > 40 — refused before any
	// exchange call or grid_bots row exists.
	if err := deploy(decimal.NewFromInt(50)); err == nil ||
		!strings.Contains(err.Error(), "конверт стопов флота") {
		t.Fatalf("envelope overflow must refuse the manual REAL deploy, got %v", err)
	}
	if got := mock.creates(); got != 0 {
		t.Fatalf("refused deploy must not reach the exchange, got %d creates", got)
	}
	var rows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE symbol = 'CANE1_USDT_PERP'
	`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused deploy must leave no grid row, got %d (%v)", rows, err)
	}

	// Round 2: free $9 of envelope (one filler gone → Σ 27); 27 + 8 = 35 ≤ 40
	// — the deploy proceeds and the manual bot stores its FULL stop (8), the
	// exact amount the gate reserved.
	if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = 'CANEF0_USDT_PERP'`); err != nil {
		t.Fatalf("free a filler slot: %v", err)
	}
	if err := deploy(decimal.NewFromInt(50)); err != nil {
		t.Fatalf("deploy under the ceiling must pass, got %v", err)
	}
	var storedStop string
	if err := pool.QueryRow(ctx, `
		SELECT max_loss_usdt::TEXT FROM grid_bots
		WHERE symbol = 'CANE1_USDT_PERP' AND account_id = $1
	`, account.ID).Scan(&storedStop); err != nil {
		t.Fatalf("load deployed manual bot: %v", err)
	}
	if got := decimal.RequireFromString(storedStop); !got.Equal(decimal.NewFromInt(8)) {
		t.Fatalf("manual bot must store the FULL stop 8 (no tranche halving), got %s", storedStop)
	}
	if got := mock.lastCreateInvestment(); got != "50" {
		t.Fatalf("envelope-passing canary must still carry investment 50 to the exchange, got %q", got)
	}
}
