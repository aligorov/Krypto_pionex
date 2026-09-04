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

// equityDetailMock serves the three documented wallet endpoints:
// GET /uapi/v1/account/detail (primary equity source), GET
// /uapi/v1/account/balances + GET /uapi/v1/account/positions (fallback).
// Modes cover the failure shapes the capture must survive or report:
//   - normal:   per-spec string fields
//   - tolerant: unquoted numbers + empty-string fields (live-API deviation)
//   - empty:    balances:[] (the silent empty-decode of the prod outage)
//   - error:    HTTP 500 on every call
type equityDetailMock struct {
	server *httptest.Server
	mu     sync.Mutex
	mode   string
	assets string
	unreal string
	avail  string
	debts  string
	// fallback payloads for the empty-detail path
	fallbackAssets string
	positionsPnL   string
}

func newEquityDetailMock(t *testing.T) *equityDetailMock {
	t.Helper()
	mock := &equityDetailMock{
		mode:           "normal",
		assets:         "500",
		unreal:         "0",
		avail:          "500",
		debts:          "0",
		fallbackAssets: "",
		positionsPnL:   "",
	}
	mux := http.NewServeMux()
	encode := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(), "data": data,
		})
	}
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		switch mock.mode {
		case "error":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, "boom")
			return
		case "empty":
			encode(w, map[string]any{"balances": []map[string]any{}})
			return
		case "tolerant":
			encode(w, map[string]any{"balances": []map[string]any{{
				"coin": "USDT", "assets": 501.25, "free": 480, "frozen": 21.25,
				"transferable": 480, "available": 480, "unrealizedPnL": -1.25,
				"totalInitialMargin": 21.25, "debts": "",
			}}})
			return
		default:
			encode(w, map[string]any{"balances": []map[string]any{{
				"coin": "USDT", "assets": mock.assets, "free": mock.avail,
				"frozen": "0", "available": mock.avail, "unrealizedPnL": mock.unreal,
				"totalInitialMargin": "0", "debts": mock.debts,
			}}})
		}
	})
	mux.HandleFunc("GET /uapi/v1/account/balances", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		if mock.mode == "error" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, "boom")
			return
		}
		balances := []map[string]any{}
		if mock.fallbackAssets != "" {
			balances = append(balances, map[string]any{
				"coin": "USDT", "free": mock.fallbackAssets, "frozen": "0", "debts": "0",
			})
		}
		encode(w, map[string]any{"balances": balances})
	})
	mux.HandleFunc("GET /uapi/v1/account/positions", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		positions := []map[string]any{}
		if mock.positionsPnL != "" {
			positions = append(positions, map[string]any{
				"symbol": "BTC_USDT_PERP", "unrealizedPnL": mock.positionsPnL,
			})
		}
		encode(w, map[string]any{"positions": positions})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *equityDetailMock) set(t *testing.T, assets, unreal, avail string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assets, m.unreal, m.avail = assets, unreal, avail
}

func (m *equityDetailMock) setMode(t *testing.T, mode string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

func (m *equityDetailMock) setFallback(t *testing.T, fallbackAssets, positionsPnL string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallbackAssets, m.positionsPnL = fallbackAssets, positionsPnL
}

// equityTestEnv wires a disposable account + worker against the mock. The
// returned cleanup removes every row the test touched (snapshots, failure
// markers, account, settings pin).
type equityTestEnv struct {
	pool     *pgxpool.Pool
	service  *Service
	worker   *Worker
	mock     *equityDetailMock
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

	mock := newEquityDetailMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-equity-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
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
		_, _ = pool.Exec(ctx, `DELETE FROM account_equity_snapshots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = 'equity'`)
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
	if err := pool.QueryRow(ctx, `
		SELECT account_id FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE autogrid_settings SET account_id = $2 WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings SET account_id = $2 WHERE scope_key = $1::VARCHAR
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
	return &equityTestEnv{
		pool: pool, service: service, worker: worker,
		mock: mock, account: account, settings: settings,
	}
}

func (env *equityTestEnv) snapshotCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM account_equity_snapshots WHERE account_id = $1
	`, env.account.ID).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return count
}

func (env *equityTestEnv) equityFailureEvents(t *testing.T) int {
	t.Helper()
	var count int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = 'equity' AND event_type = 'EQUITY_CAPTURE_FAILED'
	`).Scan(&count); err != nil {
		t.Fatalf("count equity failure events: %v", err)
	}
	return count
}

func (env *equityTestEnv) ageSnapshots(t *testing.T) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE account_equity_snapshots
		SET captured_at = NOW() - INTERVAL '6 minutes'
		WHERE account_id = $1
	`, env.account.ID); err != nil {
		t.Fatalf("age snapshots: %v", err)
	}
}

// TestAccountEquitySnapshotEpoch proves the wallet-truth chain end to end:
// (1) the worker snapshots the futures wallet equity from
// /uapi/v1/account/detail, (2) the 5-minute durable throttle holds, (3) the
// epoch PnL (equity_now − first snapshot) is derivable through the service —
// the operator's headline number can no longer be read off fee-blind bot PnL
// fields.
func TestAccountEquitySnapshotEpoch(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()

	// Anchor: $500 wallet, no floating PnL.
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 1 {
		t.Fatalf("anchor snapshot must persist, got n=%d", count)
	}
	var equity, available string
	if err := env.pool.QueryRow(ctx, `
		SELECT MAX(equity_usdt)::TEXT, MAX(available_usdt)::TEXT
		FROM account_equity_snapshots WHERE account_id = $1
	`, env.account.ID).Scan(&equity, &available); err != nil {
		t.Fatalf("load snapshots: %v", err)
	}
	if !decimal.RequireFromString(equity).Equal(decimal.NewFromInt(500)) ||
		!decimal.RequireFromString(available).Equal(decimal.NewFromInt(500)) {
		t.Fatalf("anchor snapshot wrong: equity=%s available=%s", equity, available)
	}

	// The durable 5-minute throttle: an immediate second capture must not
	// add a row even though the wallet moved.
	env.mock.set(t, "503", "0", "503")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 1 {
		t.Fatalf("5-minute throttle violated: n=%d", count)
	}

	// Age the anchor past the throttle and move the wallet by −$2.60 (fees +
	// a stop the bot PnL fields never see): equity 497.40 via assets 500 +
	// unrealized −2.60 — the unrealized leg MUST be included.
	env.ageSnapshots(t)
	env.mock.set(t, "500", "-2.6", "480")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 2 {
		t.Fatalf("second snapshot must persist after the throttle window, got n=%d", count)
	}
	if err := env.pool.QueryRow(ctx, `
		SELECT equity_usdt::TEXT FROM account_equity_snapshots
		WHERE account_id = $1 ORDER BY captured_at DESC LIMIT 1
	`, env.account.ID).Scan(&equity); err != nil {
		t.Fatalf("load newest snapshot: %v", err)
	}
	if got := decimal.RequireFromString(equity); !got.Equal(decimal.NewFromFloat(497.4)) {
		t.Fatalf("equity must net assets + unrealizedPnL (500 − 2.6), got %s", equity)
	}

	// The service-side epoch accounting: equity_now − first snapshot.
	summary, err := env.service.AccountEquityEpoch(ctx)
	if err != nil {
		t.Fatalf("epoch summary: %v", err)
	}
	if summary == nil {
		t.Fatal("epoch summary must exist after snapshots")
	}
	if !summary.EquityStartUSDT.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("epoch anchor must be the first snapshot (500), got %s", summary.EquityStartUSDT)
	}
	if !summary.EpochPnLUSDT.Equal(decimal.NewFromFloat(-2.6)) {
		t.Fatalf("epoch PnL (wallet truth) must be −2.60, got %s", summary.EpochPnLUSDT)
	}
	if summary.Snapshots != 2 {
		t.Fatalf("summary must count both snapshots, got %d", summary.Snapshots)
	}
	if !summary.UnrealizedPnLUSDT.Equal(decimal.NewFromFloat(-2.6)) {
		t.Fatalf("exchange unrealized must mirror the newest snapshot (−2.6), got %s", summary.UnrealizedPnLUSDT)
	}
	if summary.CapturedAt.IsZero() {
		t.Fatal("summary must expose the newest capture time")
	}

	// An empty futures wallet still adds no snapshot (fail-open), but the
	// death is no longer silent: an EQUITY_CAPTURE_FAILED marker lands.
	env.ageSnapshots(t)
	env.mock.set(t, "0", "0", "0")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 2 {
		t.Fatalf("an empty wallet must add no snapshot, got n=%d", count)
	}
	if events := env.equityFailureEvents(t); events != 1 {
		t.Fatalf("empty wallet must leave exactly one EQUITY_CAPTURE_FAILED marker, got %d", events)
	}
}

// TestAccountEquityCaptureObservability pins the v2.0.80 no-silent-death
// contract: a failing fetch and an empty decode each produce a durable
// EQUITY_CAPTURE_FAILED marker (deduped to one per hour) and zero snapshot
// rows — the prod state that hid for weeks behind a bare Warn.
func TestAccountEquityCaptureObservability(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()

	// Fetch error → no snapshot, one durable marker.
	env.mock.setMode(t, "error")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 0 {
		t.Fatalf("failed fetch must add no snapshot, got n=%d", count)
	}
	if events := env.equityFailureEvents(t); events != 1 {
		t.Fatalf("failed fetch must leave one EQUITY_CAPTURE_FAILED marker, got %d", events)
	}

	// The 1-hour dedup: a second failing pass adds no second marker.
	env.worker.captureAccountEquity(ctx, *env.settings)
	if events := env.equityFailureEvents(t); events != 1 {
		t.Fatalf("dedup violated: %d markers within the hour", events)
	}

	// Empty decode with no fallback data → no snapshot, marker fires (the
	// first marker is already <1h old, so the count stays deduped — the
	// invariant is the marker EXISTS and no row landed).
	env.mock.setMode(t, "empty")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 0 {
		t.Fatalf("empty decode must add no snapshot, got n=%d", count)
	}
	if events := env.equityFailureEvents(t); events != 1 {
		t.Fatalf("unexpected marker count after empty decode: %d", events)
	}
	var reason string
	if err := env.pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = 'equity' AND event_type = 'EQUITY_CAPTURE_FAILED'
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&reason); err != nil {
		t.Fatalf("load marker reason: %v", err)
	}
	if reason != "FETCH_FAILED" && reason != "EMPTY_DECODE" {
		t.Fatalf("marker must carry a machine reason, got %q", reason)
	}
}

// TestAccountEquityEmptyDecodeFallback: when account/detail decodes to an
// empty wallet, the documented balances+positions endpoints reconstruct the
// equity (assets = free+frozen, floating = Σ position unrealizedPnL) so the
// ledger keeps flowing; with no fallback data either, the capture reports
// instead of silently doing nothing.
func TestAccountEquityEmptyDecodeFallback(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()

	// detail empty, balances carries $400 free, one position floats −$1.5.
	env.mock.setMode(t, "empty")
	env.mock.setFallback(t, "400", "-1.5")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 1 {
		t.Fatalf("fallback must snapshot the wallet, got n=%d", count)
	}
	var equity, assets, unreal string
	if err := env.pool.QueryRow(ctx, `
		SELECT equity_usdt::TEXT, assets_usdt::TEXT, unrealized_pnl_usdt::TEXT
		FROM account_equity_snapshots WHERE account_id = $1
	`, env.account.ID).Scan(&equity, &assets, &unreal); err != nil {
		t.Fatalf("load fallback snapshot: %v", err)
	}
	if !decimal.RequireFromString(assets).Equal(decimal.NewFromInt(400)) ||
		!decimal.RequireFromString(unreal).Equal(decimal.NewFromFloat(-1.5)) ||
		!decimal.RequireFromString(equity).Equal(decimal.NewFromFloat(398.5)) {
		t.Fatalf("fallback snapshot wrong: equity=%s assets=%s unreal=%s", equity, assets, unreal)
	}
	if events := env.equityFailureEvents(t); events != 0 {
		t.Fatalf("a successful fallback must not raise the failure marker, got %d", events)
	}

	// detail empty AND fallback empty → no snapshot, marker reported.
	env.ageSnapshots(t)
	env.mock.setFallback(t, "", "")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 1 {
		t.Fatalf("empty everywhere must add no snapshot, got n=%d", count)
	}
	if events := env.equityFailureEvents(t); events != 1 {
		t.Fatalf("empty everywhere must raise exactly one marker, got %d", events)
	}
}

// TestAccountEquityTolerantDecode: the live API sending unquoted numbers
// and empty strings where the docs promise decimal strings must still
// produce a snapshot — this is the deviation class the v2.0.75 decoder was
// defenseless against.
func TestAccountEquityTolerantDecode(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()

	env.mock.setMode(t, "tolerant")
	env.worker.captureAccountEquity(ctx, *env.settings)
	if count := env.snapshotCount(t); count != 1 {
		t.Fatalf("tolerant decode must snapshot, got n=%d", count)
	}
	var equity string
	if err := env.pool.QueryRow(ctx, `
		SELECT equity_usdt::TEXT FROM account_equity_snapshots
		WHERE account_id = $1 ORDER BY captured_at DESC LIMIT 1
	`, env.account.ID).Scan(&equity); err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if got := decimal.RequireFromString(equity); !got.Equal(decimal.NewFromFloat(500)) {
		t.Fatalf("tolerant equity must be 501.25 − 1.25 = 500, got %s", equity)
	}
	if events := env.equityFailureEvents(t); events != 0 {
		t.Fatalf("tolerant decode must not raise the failure marker, got %d", events)
	}
}

// TestAccountEquityEpochEmptyLedger: the endpoint's empty-table contract —
// the NULL-row scan must answer a nil summary (HTTP 200 available:false),
// never a 500.
func TestAccountEquityEpochEmptyLedger(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()

	summary, err := env.service.AccountEquityEpoch(ctx)
	if err != nil {
		t.Fatalf("empty ledger must not error (endpoint 500 regression), got: %v", err)
	}
	if summary != nil {
		t.Fatalf("empty ledger must answer a nil summary (available:false), got %+v", summary)
	}
}
