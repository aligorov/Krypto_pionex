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

// equityAggregateMock serves the exchange surfaces the bot-aggregate capture
// touches: the grid-order detail endpoint (running remote PnL truth for the
// manage loop) and /uapi/v1/account/detail (the wallet leg, structurally
// zero on an isolated-grid account — the prod shape v2.0.83 was built for).
// Modes:
//   - normal: detail answers all-zero USDT (the isolated norm, NOT an alarm)
//   - error:  HTTP 500 on account/detail (a real fetch failure → alarm)
type equityAggregateMock struct {
	server *httptest.Server
	mu     sync.Mutex
	mode   string
	// walletUSDT optionally overrides the USDT row of account/detail.
	walletUSDT string
}

func newEquityAggregateMock(t *testing.T) *equityAggregateMock {
	t.Helper()
	mock := &equityAggregateMock{mode: "normal", walletUSDT: "0"}
	gridPayload := func(reduce string) map[string]any {
		return map[string]any{
			"status": "running", "reasonBy": "",
			"top": "1.8", "bottom": "1.4", "row": 20,
			"gridType": "arithmetic", "trend": "no_trend", "leverage": 1,
			"position": "0", "positionOpenPrice": "1.5",
			"profitReduce": reduce, "profitWithdrawn": "0",
			"profitExited": "0", "fundingFeePayment": "0",
			"riskStatus": "NORMAL",
		}
	}
	respond := func(w http.ResponseWriter, payload any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		if mock.mode == "error" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, "boom")
			return
		}
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"balances": []map[string]any{{
				"coin": "USDT", "assets": mock.walletUSDT, "free": mock.walletUSDT,
				"frozen": "0", "available": mock.walletUSDT, "unrealizedPnL": "0",
				"totalInitialMargin": "0", "debts": "0",
			}}},
		})
	})
	// Grid-order detail: every EQ-* bot answers a running grid with the
	// profitReduce the test configures per bot id.
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		reduce := "0"
		switch r.URL.Query().Get("buOrderId") {
		case "EQ-R1":
			reduce = "1.0"
		case "EQ-R2":
			reduce = "2.0"
		}
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"), "status": "running", "reasonBy": "",
				"buOrderData": gridPayload(reduce),
			},
		})
	})
	mux.HandleFunc("GET /api/v1/bot/orders", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"orders": []any{}},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": "EQR1_USDT_PERP", "indexPrice": "1.5", "markPrice": "1.5"},
				{"symbol": "EQR2_USDT_PERP", "indexPrice": "1.5", "markPrice": "1.5"},
			}},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *equityAggregateMock) setMode(t *testing.T, mode string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

// equityTestEnv wires a disposable account + worker against the mock. The
// returned cleanup removes every row the test touched (bots, telemetry,
// snapshots, events, epoch anchor, settings pin, account).
type equityTestEnv struct {
	pool     *pgxpool.Pool
	service  *Service
	worker   *Worker
	mock     *equityAggregateMock
	account  *accounts.Account
	settings *Settings
}

func newEquityTestEnv(t *testing.T) *equityTestEnv {
	t.Helper()
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newEquityAggregateMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-equity-test-" + time.Now().Format("150405.000000000")
	// Other integration suites drive reconcileAndManage against mocks that
	// do not serve /uapi/v1/account/detail — their captures legitimately
	// leave FETCH_FAILED markers (bot_id='equity', 1h dedup, GLOBAL). Clear
	// them so this suite's zero-alarm assertions are hermetic.
	_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = 'equity'`)
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-equity-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-equity-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-equity-test%'`)
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
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM account_equity_snapshots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = 'equity'`)
		_, _ = pool.Exec(ctx, `DELETE FROM app_config WHERE key = 'pnl_epoch_started_at'`)
		_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
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
		_, _ = pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4, ai_autotune_enabled = $5
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode, savedAutotune)
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

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")
	return &equityTestEnv{
		pool: pool, service: service, worker: worker,
		mock: mock, account: account, settings: settings,
	}
}

// pinEpochAnchor rewrites app_config('pnl_epoch_started_at') so seeded bots
// with NOW() created_at fall inside the epoch deterministically.
func (env *equityTestEnv) pinEpochAnchor(t *testing.T, anchor time.Time) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO app_config (key, value, description)
		VALUES ('pnl_epoch_started_at', to_jsonb($1::TEXT), 'test anchor')
		ON CONFLICT (key) DO UPDATE SET value = to_jsonb($1::TEXT)
	`, anchor.Format(time.RFC3339)); err != nil {
		t.Fatalf("pin epoch anchor: %v", err)
	}
}

func (env *equityTestEnv) seedBot(t *testing.T, symbol, remoteID, status string, investment, realized, unrealized any) string {
	t.Helper()
	ctx := context.Background()
	var botID string
	isClosed := status == "STOPPED" || status == "COMPLETED" || status == "CANCELLED" || status == "LIQUIDATED"
	// Closed seeds carry the finalProfitSource marker every real settle
	// writes — without it the 48h backfill would re-settle them against the
	// mock's running payload and overwrite the fixture figures.
	modelState := "'{}'::JSONB"
	if isClosed {
		modelState = `CASE WHEN $8::NUMERIC IS NULL THEN '{"finalProfitSource":"refused_profit_exited"}'::JSONB
			ELSE '{"finalProfitSource":"profit_exited"}'::JSONB END`
	}
	err := env.pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			pnl_target_usdt, max_loss_usdt, realized_pnl_usdt, unrealized_pnl_usdt,
			closed_at, model_state
		) VALUES (
			$1, $2, $3, $4, 'NEUTRAL',
			'ARITHMETIC', 1.4, 1.8, 20, 1,
			$5, 0, $6, 'REAL', 'REMOTE_ID_PERSISTED', $7,
			1000, 1000, $8, $9,
			CASE WHEN $10::BOOLEAN THEN NOW() ELSE NULL END,
			`+modelState+`
		)
		RETURNING id
	`, env.account.ID, env.settings.ID, symbol, status, investment,
		"itest-"+time.Now().Format("150405.000000000")+"-"+symbol+"-"+remoteID,
		remoteID, realized, unrealized, isClosed,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot %s: %v", symbol, err)
	}
	return botID
}

func (env *equityTestEnv) seedTelemetry(t *testing.T, botID string, total string, capturedAt time.Time) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO bot_telemetry (bot_id, bot_number, symbol, captured_at, price, total_pnl)
		VALUES ($1, 0, 'TEST', $2, 1.5, $3::NUMERIC)
	`, botID, capturedAt, total); err != nil {
		t.Fatalf("seed telemetry: %v", err)
	}
}

func (env *equityTestEnv) snapshots(t *testing.T) int {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM account_equity_snapshots
		WHERE account_id = $1 AND source = 'bot_aggregate'
	`, env.account.ID).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return count
}

func (env *equityTestEnv) equityEvents(t *testing.T, eventType string) int {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = 'equity' AND event_type = $1
	`, eventType).Scan(&count); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return count
}

func (env *equityTestEnv) ageSnapshots(t *testing.T) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE account_equity_snapshots
		SET captured_at = NOW() - INTERVAL '6 minutes'
		WHERE account_id = $1 AND source = 'bot_aggregate'
	`, env.account.ID); err != nil {
		t.Fatalf("age snapshots: %v", err)
	}
}

func mustDec(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return parsed
}

// TestBotAggregateSnapshotAndEpoch is the end-to-end proof of the v2.0.83
// model: after a reconcile pass over two running bots (remote profitReduce
// 1.0/2.0, floating −0.5/+0.25, investment 50/100 — floating is seeded
// directly because the mock answers a flat position) and a closed-of-epoch
// fleet (known +1.5, telemetry-estimated −2.5, one unknown), the snapshot
// row and the endpoint breakdown must carry exactly:
//
//	assets_usdt       = 150        (Σ running investment)
//	unrealized_pnl    = −0.25      (Σ running floating)
//	equity_usdt       = 0 + 150 + 2.75 = 152.75
//	running_pnl       = 3.0 + (−0.25) = 2.75
//	closed_known      = 1.5, closed_estimated = −2.5, unknown_count = 1
//	epoch_pnl         = 2.75 + 1.5 − 2.5 = 1.75
func TestBotAggregateSnapshotAndEpoch(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()
	env.pinEpochAnchor(t, time.Now().UTC().Add(-time.Hour))

	// Running fleet: the manage pass resyncs realized from remote
	// profitReduce (1.0 / 2.0 — funding reported 0) and keeps the seeded
	// floating marks (flat position in the mock: position=0 → floating
	// would be zeroed, so seed the floating through a second pass below).
	runningA := env.seedBot(t, "EQR1_USDT_PERP", "EQ-R1", "RUNNING", 50, 0, "-0.5")
	runningB := env.seedBot(t, "EQR2_USDT_PERP", "EQ-R2", "RUNNING", 100, 0, "0.25")

	// Closed fleet of the epoch: a settled final, a refused settle with a
	// telemetry trace, and a refused settle with no trace at all.
	knownBot := env.seedBot(t, "EQC1_USDT_PERP", "EQ-C1", "STOPPED", 50, "1.5", 0)
	_ = knownBot
	estimatedBot := env.seedBot(t, "EQC2_USDT_PERP", "EQ-C2", "STOPPED", 50, nil, 0)
	env.seedTelemetry(t, estimatedBot, "-2.5", time.Now().UTC().Add(-time.Minute))
	unknownBot := env.seedBot(t, "EQC3_USDT_PERP", "EQ-C3", "STOPPED", 50, nil, 0)
	_ = unknownBot

	if _, err := env.worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	// The manage pass resynced realized from remote profitReduce and — flat
	// position — zeroed the floating column. Re-seed the floating marks and
	// write them durably through one more pass cycle's PnL persist by
	// setting them directly (the floating leg of the aggregate reads the
	// persisted column; remote position=0 is the "no floating" truth, the
	// seeded marks emulate a live mark-price gap).
	for _, seed := range []struct {
		botID      string
		unrealized string
	}{{runningA, "-0.5"}, {runningB, "0.25"}} {
		if _, err := env.pool.Exec(ctx, `
			UPDATE grid_bots SET unrealized_pnl_usdt = $2::NUMERIC WHERE id = $1
		`, seed.botID, seed.unrealized); err != nil {
			t.Fatalf("re-seed floating: %v", err)
		}
	}
	env.ageSnapshots(t)
	env.worker.captureBotAggregateEquity(ctx, *env.settings)

	// (a) The snapshot row: bot-aggregate semantics.
	var equity, assets, avail, unrealized string
	if err := env.pool.QueryRow(ctx, `
		SELECT equity_usdt::TEXT, assets_usdt::TEXT, available_usdt::TEXT, unrealized_pnl_usdt::TEXT
		FROM account_equity_snapshots
		WHERE account_id = $1 AND source = 'bot_aggregate'
		ORDER BY captured_at DESC LIMIT 1
	`, env.account.ID).Scan(&equity, &assets, &avail, &unrealized); err != nil {
		t.Fatalf("load aggregate snapshot: %v", err)
	}
	if !mustDec(t, assets).Equal(mustDec(t, "150")) {
		t.Fatalf("assets_usdt = Σ running investment (50+100), got %s", assets)
	}
	if !mustDec(t, unrealized).Equal(mustDec(t, "-0.25")) {
		t.Fatalf("unrealized_pnl_usdt = Σ running floating (−0.5+0.25), got %s", unrealized)
	}
	if !mustDec(t, equity).Equal(mustDec(t, "152.75")) {
		t.Fatalf("equity_usdt = wallet 0 + investment 150 + running PnL 2.75, got %s", equity)
	}
	if !mustDec(t, avail).IsZero() {
		t.Fatalf("available_usdt mirrors the empty wallet, got %s", avail)
	}

	// (b)+(c) The endpoint breakdown.
	summary, err := env.service.AccountEquityEpoch(ctx)
	if err != nil {
		t.Fatalf("epoch summary: %v", err)
	}
	if summary == nil {
		t.Fatal("epoch summary must always exist on success")
	}
	if !summary.RunningPnLUSDT.Equal(mustDec(t, "2.75")) {
		t.Fatalf("running_pnl = 3.0 realized + (−0.25) floating, got %s", summary.RunningPnLUSDT)
	}
	if !summary.RunningFloatingUSDT.Equal(mustDec(t, "-0.25")) {
		t.Fatalf("running floating leg, got %s", summary.RunningFloatingUSDT)
	}
	if !summary.RunningInvestmentUSDT.Equal(mustDec(t, "150")) {
		t.Fatalf("running investment leg, got %s", summary.RunningInvestmentUSDT)
	}
	if summary.RunningBots != 2 {
		t.Fatalf("running bots, got %d", summary.RunningBots)
	}
	if !summary.ClosedKnownUSDT.Equal(mustDec(t, "1.5")) {
		t.Fatalf("closed_known = the settled final, got %s", summary.ClosedKnownUSDT)
	}
	if !summary.ClosedEstimatedUSDT.Equal(mustDec(t, "-2.5")) {
		t.Fatalf("closed_estimated = telemetry fallback, got %s", summary.ClosedEstimatedUSDT)
	}
	if summary.UnknownCount != 1 {
		t.Fatalf("unknown_count = 1 (NULL final, no telemetry), got %d", summary.UnknownCount)
	}
	if !summary.EpochPnLUSDT.Equal(mustDec(t, "1.75")) {
		t.Fatalf("epoch PnL = 2.75 + 1.5 − 2.5, got %s", summary.EpochPnLUSDT)
	}
	if summary.Snapshots < 1 {
		t.Fatalf("summary must count the bot_aggregate snapshots, got %d", summary.Snapshots)
	}

	// The 5-minute durable throttle: an immediate second capture must not
	// add a row.
	env.worker.captureBotAggregateEquity(ctx, *env.settings)
	if got := env.snapshots(t); got != 2 { // reconcile wrote one, capture wrote one
		t.Fatalf("5-minute throttle violated: n=%d", got)
	}

	// An account answering zero (the isolated norm) must NOT alarm — the
	// aggregate landed, zero alarm events, one hourly heartbeat.
	if got := env.equityEvents(t, "EQUITY_CAPTURE_FAILED"); got != 0 {
		t.Fatalf("an empty wallet is the isolated norm — no alarm, got %d", got)
	}
	if got := env.equityEvents(t, "EQUITY_SNAPSHOT"); got != 1 {
		t.Fatalf("exactly one hourly EQUITY_SNAPSHOT heartbeat expected, got %d", got)
	}
}

// TestBotAggregateTelemetryFallbackWindow pins the fallback ladder: a fresh
// row (<5 min before close) wins over an older one; with nothing fresh the
// nearest-earlier row still estimates; with no telemetry at all the bot is
// counted unknown and excluded from the epoch sum.
func TestBotAggregateTelemetryFallbackWindow(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()
	env.pinEpochAnchor(t, time.Now().UTC().Add(-time.Hour))

	freshBot := env.seedBot(t, "EQW1_USDT_PERP", "EQ-W1", "STOPPED", 50, nil, 0)
	// One stale row (2h before close) and one fresh row (1 min before close):
	// the fresh row must win even though the stale one exists.
	env.seedTelemetry(t, freshBot, "-9.9", time.Now().UTC().Add(-2*time.Hour))
	if _, err := env.pool.Exec(ctx, `
		UPDATE grid_bots SET closed_at = NOW() - INTERVAL '1 minute' WHERE id = $1
	`, freshBot); err != nil {
		t.Fatalf("shift closed_at: %v", err)
	}
	env.seedTelemetry(t, freshBot, "-2.5", time.Now().UTC().Add(-90*time.Second))

	staleBot := env.seedBot(t, "EQW2_USDT_PERP", "EQ-W2", "STOPPED", 50, nil, 0)
	// Only a row from 2h before close: nearest-earlier must still estimate.
	if _, err := env.pool.Exec(ctx, `
		UPDATE grid_bots SET closed_at = NOW() WHERE id = $1
	`, staleBot); err != nil {
		t.Fatalf("shift closed_at: %v", err)
	}
	env.seedTelemetry(t, staleBot, "-0.75", time.Now().UTC().Add(-2*time.Hour))

	botAfterClose := env.seedBot(t, "EQW3_USDT_PERP", "EQ-W3", "STOPPED", 50, nil, 0)
	// A row captured AFTER close must never be used (post-close noise).
	env.seedTelemetry(t, botAfterClose, "-5.0", time.Now().UTC().Add(time.Minute))

	breakdown, err := ComputeEquityBreakdown(ctx, env.pool, env.account.ID, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if !breakdown.ClosedEstimated.Equal(mustDec(t, "-3.25")) {
		t.Fatalf("estimated = fresh −2.5 + nearest-earlier −0.75, got %s", breakdown.ClosedEstimated)
	}
	if breakdown.ClosedEstimatedBots != 2 {
		t.Fatalf("estimated bots, got %d", breakdown.ClosedEstimatedBots)
	}
	if breakdown.UnknownCount != 1 {
		t.Fatalf("the after-close-only bot must be unknown, got %d", breakdown.UnknownCount)
	}
	if !breakdown.EpochPnL().Equal(mustDec(t, "-3.25")) {
		t.Fatalf("epoch PnL excludes unknowns, got %s", breakdown.EpochPnL())
	}
}

// TestBotAggregateEpochBoundaryFilter: bots created BEFORE the epoch anchor
// and PAPER bots never enter the aggregate — the anchor decides the fleet.
func TestBotAggregateEpochBoundaryFilter(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()
	env.pinEpochAnchor(t, time.Now().UTC().Add(-time.Hour))

	inEpoch := env.seedBot(t, "EQF1_USDT_PERP", "EQ-F1", "STOPPED", 50, "1.5", 0)
	_ = inEpoch
	beforeEpoch := env.seedBot(t, "EQF2_USDT_PERP", "EQ-F2", "STOPPED", 50, "-100", 0)
	if _, err := env.pool.Exec(ctx, `
		UPDATE grid_bots SET created_at = NOW() - INTERVAL '48 hours' WHERE id = $1
	`, beforeEpoch); err != nil {
		t.Fatalf("age bot out of epoch: %v", err)
	}
	paperBot := env.seedBot(t, "EQF3_USDT_PERP", "EQ-F3", "STOPPED", 50, "-50", 0)
	if _, err := env.pool.Exec(ctx, `
		UPDATE grid_bots SET execution_mode = 'PAPER' WHERE id = $1
	`, paperBot); err != nil {
		t.Fatalf("flag paper bot: %v", err)
	}

	breakdown, err := ComputeEquityBreakdown(ctx, env.pool, env.account.ID, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if !breakdown.ClosedKnown.Equal(mustDec(t, "1.5")) {
		t.Fatalf("only the in-epoch REAL final counts, got %s", breakdown.ClosedKnown)
	}
}

// TestBotAggregateCaptureAlarms pins the v2.0.83 alarm contract: an
// account-empty answer is the isolated norm (snapshot lands, no alarm), a
// real fetch failure alarms durably with 1-hour dedup and writes nothing.
func TestBotAggregateCaptureAlarms(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()
	env.pinEpochAnchor(t, time.Now().UTC().Add(-time.Hour))

	// account/detail → HTTP 500: a real failure. No snapshot, one marker.
	env.mock.setMode(t, "error")
	env.worker.captureBotAggregateEquity(ctx, *env.settings)
	if got := env.snapshots(t); got != 0 {
		t.Fatalf("failed fetch must add no snapshot, got n=%d", got)
	}
	if got := env.equityEvents(t, "EQUITY_CAPTURE_FAILED"); got != 1 {
		t.Fatalf("failed fetch must leave one marker, got %d", got)
	}
	// 1-hour dedup on the alarm.
	env.worker.captureBotAggregateEquity(ctx, *env.settings)
	if got := env.equityEvents(t, "EQUITY_CAPTURE_FAILED"); got != 1 {
		t.Fatalf("dedup violated: %d markers within the hour", got)
	}

	// account/detail answers zero USDT (isolated norm): the snapshot lands
	// and no NEW alarm appears.
	env.mock.setMode(t, "normal")
	env.worker.captureBotAggregateEquity(ctx, *env.settings)
	if got := env.snapshots(t); got != 1 {
		t.Fatalf("empty wallet is the norm — snapshot must land, got n=%d", got)
	}
	if got := env.equityEvents(t, "EQUITY_CAPTURE_FAILED"); got != 1 {
		t.Fatalf("empty wallet must not alarm, got %d markers", got)
	}
	var reason string
	if err := env.pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = 'equity' AND event_type = 'EQUITY_CAPTURE_FAILED'
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&reason); err != nil {
		t.Fatalf("load marker reason: %v", err)
	}
	if reason != "FETCH_FAILED" {
		t.Fatalf("marker must carry a machine reason, got %q", reason)
	}
}

// TestBotAggregateEpochAnchorConfig: the durable anchor drives the fleet
// boundary — moving app_config('pnl_epoch_started_at') past every bot
// empties the epoch; a malformed value is an error, never a silent reset.
func TestBotAggregateEpochAnchorConfig(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()
	env.pinEpochAnchor(t, time.Now().UTC().Add(-time.Hour))
	env.seedBot(t, "EQA1_USDT_PERP", "EQ-A1", "STOPPED", 50, "1.5", 0)

	summary, err := env.service.AccountEquityEpoch(ctx)
	if err != nil {
		t.Fatalf("epoch summary: %v", err)
	}
	if !summary.ClosedKnownUSDT.Equal(mustDec(t, "1.5")) {
		t.Fatalf("in-epoch final must count, got %s", summary.ClosedKnownUSDT)
	}

	// Move the anchor into the future: the epoch empties (zeros, still
	// available — an empty epoch is not an alarm).
	env.pinEpochAnchor(t, time.Now().UTC().Add(time.Hour))
	summary, err = env.service.AccountEquityEpoch(ctx)
	if err != nil {
		t.Fatalf("epoch summary after re-anchor: %v", err)
	}
	if summary == nil || !summary.EpochPnLUSDT.IsZero() {
		t.Fatalf("re-anchored epoch must answer zeroes, got %+v", summary)
	}

	// Malformed anchor: surfaced as an error, never silently re-anchored.
	if _, err := env.pool.Exec(ctx, `
		UPDATE app_config SET value = '"not-a-timestamp"' WHERE key = 'pnl_epoch_started_at'
	`); err != nil {
		t.Fatalf("break anchor: %v", err)
	}
	if _, err := env.service.AccountEquityEpoch(ctx); err == nil {
		t.Fatal("malformed anchor must surface an error")
	}
}
