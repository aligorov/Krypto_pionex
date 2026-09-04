package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// realDeployExchangeMock serves every endpoint a deployReal round touches:
// public tickers/klines (revalidation, HAR geometry, vol expansion), the
// common PERP symbol list CreateGridBot validates against, and the signed
// futuresGrid checkParams/create calls. checkParams and create can each be
// switched to the exchange's symbol-maintenance refusal: HTTP 403 whose body
// is deliberately NOT valid JSON, so the client wraps it as "invalid JSON
// response: <snippet>" with the reason code surviving inside the snippet —
// the production shape of the maintenance error.
type realDeployExchangeMock struct {
	server      *httptest.Server
	createCalls atomic.Int64
	checkMaint  atomic.Bool
	createMaint atomic.Bool
	// v2.0.74: raw 403 bodies for the broadened forbidden classifier — set
	// one to make the matching endpoint answer 403 with exactly that body
	// (checkBody wins over checkMaint, createBody over createMaint).
	checkBody  atomic.Value
	createBody atomic.Value
}

func maintenanceBody() string {
	// Truncated JSON object: envelope unmarshalling fails, the reason rides
	// in the client's body snippet (first 160 bytes, reason fits).
	return `{"reason":"` + pionexSymbolMaintenanceReason + `","result":false`
}

func newRealDeployExchangeMock(t *testing.T, symbols ...string) *realDeployExchangeMock {
	t.Helper()
	mock := &realDeployExchangeMock{}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, payload any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}
	symbolSet := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		symbolSet[symbol] = true
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
	// 60 candles oscillating around 100: the fresh-trend revalidation reads
	// RANGE/no_trend, the fresh price equals the scan price, and both the
	// HAR fit and the 24h vol-expansion baseline stay deterministic.
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
		list := make([]map[string]any, 0, len(symbolSet))
		for symbol := range symbolSet {
			list = append(list, map[string]any{
				"symbol": symbol, "type": "PERP", "enable": true, "status": "TRADING",
			})
		}
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"symbols": list},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/checkParams", func(w http.ResponseWriter, _ *http.Request) {
		if body, ok := mock.checkBody.Load().(string); ok && body != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(body))
			return
		}
		if mock.checkMaint.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(maintenanceBody()))
			return
		}
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"minInvestment": "30"},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/create", func(w http.ResponseWriter, _ *http.Request) {
		mock.createCalls.Add(1)
		if body, ok := mock.createBody.Load().(string); ok && body != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(body))
			return
		}
		if mock.createMaint.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(maintenanceBody()))
			return
		}
		writeJSON(w, map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"buOrderId": "REALSKIP-1"},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// realDeployHarness wires a Worker + Service against a disposable DB, one
// VERIFIED account (enabled + read permission + last_verified_at, the exact
// shape resolveAccount and realExecutionAllowed demand) and the mock
// exchange, with the durable REAL gates pinned for the round.
type realDeployHarness struct {
	pool     *pgxpool.Pool
	worker   *Worker
	service  *Service
	settings *Settings
	account  accounts.Account
	mock     *realDeployExchangeMock
}

func newRealDeployHarness(t *testing.T, maxActiveBots int, symbols ...string) *realDeployHarness {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, integrationDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	snapshotManualSettings(t, pool, settings.ID)

	accountName := "integration-real-skip-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-real-skip-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-real-skip-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-real-skip-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-real-skip-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		// Detach the settings pointer BEFORE deleting the account (FK), and
		// clear any last_error the deploy round wrote (the snapshot restore
		// does not own that column).
		if _, err := pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("detach settings account: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE autogrid_settings SET last_error = NULL WHERE scope_key = 'default'`); err != nil {
			t.Errorf("clear last_error: %v", err)
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
	// Create() leaves the row UNVERIFIED; mark it the way Verify() would.
	if _, err := pool.Exec(ctx, `
		UPDATE pionex_accounts
		SET is_enabled = true, has_read_permission = true, last_verified_at = NOW()
		WHERE id = $1
	`, account.ID); err != nil {
		t.Fatalf("verify account: %v", err)
	}

	mock := newRealDeployExchangeMock(t, symbols...)
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, max_active_bots = $3, last_error = NULL
		WHERE id = $1
	`, settings.ID, account.ID, maxActiveBots); err != nil {
		t.Fatalf("pin settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = false, max_daily_loss_usd = 100000,
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
	reloaded, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	return &realDeployHarness{
		pool: pool, worker: worker, service: service,
		settings: reloaded, account: *account, mock: mock,
	}
}

// seedRunningRealBot inserts one RUNNING native grid row for the account.
func (h *realDeployHarness) seedRunningRealBot(t *testing.T, symbol string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id
		) VALUES (
			$1, $2, $3, 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $4, 'REAL', 'REMOTE_ID_PERSISTED', $5
		)
	`, h.account.ID, h.settings.ID, symbol,
		"itest-"+time.Now().Format("150405.000000000"),
		fmt.Sprintf("RSKIP-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("seed running real bot %s: %v", symbol, err)
	}
}

// seedAcceptedCandidate mints a scan run with one ACCEPTED RANGE candidate,
// plus passing walk-forward verdicts so the backtest gate never shadows the
// branch under test.
func (h *realDeployHarness) seedAcceptedCandidate(t *testing.T, symbol string) string {
	t.Helper()
	ctx := context.Background()
	tradedTF := normalizeBacktestTF(h.settings.CandleInterval)
	for _, tf := range append([]string{tradedTF}, neighborBacktestTFs(tradedTF)...) {
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO backtest_jobs (symbol, interval, status, result, finished_at)
			VALUES ($1, $2, 'DONE',
			        '{"folds": 4, "oos_return_pct": 1.2, "oos_max_drawdown": 0.05, "round_trips": 100, "stop_hits": 0}'::jsonb,
			        NOW())
		`, symbol, tf); err != nil {
			t.Fatalf("seed backtest job %s: %v", tf, err)
		}
	}
	var scanID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, current_price, lower_price, upper_price,
			grid_num, recommended_trend, model_assumptions
		) VALUES (
			$1, $2, 'ACCEPTED', 100, 90, 110,
			10, 'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		)
	`, scanID, symbol); err != nil {
		t.Fatalf("insert candidate %s: %v", symbol, err)
	}
	return scanID
}

// candidateRow loads decision + rejection reason for one scan candidate.
func (h *realDeployHarness) candidateRow(t *testing.T, scanID, symbol string) (string, string) {
	t.Helper()
	var decision, reason string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT decision, COALESCE(rejection_reason, '') FROM autogrid_candidates
		WHERE scan_id = $1 AND symbol = $2
	`, scanID, symbol).Scan(&decision, &reason); err != nil {
		t.Fatalf("load candidate %s: %v", symbol, err)
	}
	return decision, reason
}

func (h *realDeployHarness) cleanupSymbol(t *testing.T, symbol string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := h.pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup backtest jobs: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $2 AND symbol = $1)`, symbol, h.account.ID); err != nil {
			t.Errorf("cleanup telemetry: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $2 AND symbol = $1`, symbol, h.account.ID); err != nil {
			t.Errorf("cleanup grid bots: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id IN (
			SELECT scan_id FROM autogrid_candidates WHERE symbol = $1)`, symbol); err != nil {
			t.Errorf("cleanup scan runs: %v", err)
		}
	})
}

// (a) A full portfolio must reject ACCEPTED candidates with the visible
// «портфель полон (N/N)» reason instead of silently continuing — the prod
// 2026-09-03 incident: the fleet sat at 5/5 for hours with every candidate
// ACCEPTED and no one wrote why.
func TestDeployRealPortfolioFullSkipIsVisible(t *testing.T) {
	h := newRealDeployHarness(t, 5, "SLOT_USDT_PERP")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		h.seedRunningRealBot(t, fmt.Sprintf("SLOTFILL%d_USDT_PERP", i))
	}
	h.cleanupSymbol(t, "SLOT_USDT_PERP")
	scanID := h.seedAcceptedCandidate(t, "SLOT_USDT_PERP")

	if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
		t.Fatalf("deployReal: %v", err)
	}
	decision, reason := h.candidateRow(t, scanID, "SLOT_USDT_PERP")
	if decision != "REJECTED" {
		t.Fatalf("full portfolio must reject the candidate, got %s", decision)
	}
	if !strings.Contains(reason, "портфель полон (5/5)") {
		t.Fatalf("rejection must name the full portfolio (5/5), got %q", reason)
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE account_id = $1 AND symbol = 'SLOT_USDT_PERP'
	`, h.account.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("no grid row may be created for the rejected candidate, got %d (%v)", rows, err)
	}
	if got := h.mock.createCalls.Load(); got != 0 {
		t.Fatalf("exchange create must not be called on a full portfolio, got %d", got)
	}
}

// (b) A candidate whose symbol already runs a grid must be rejected with the
// visible «символ уже в работе» reason instead of silently continuing.
func TestDeployRealDuplicateSymbolSkipIsVisible(t *testing.T) {
	h := newRealDeployHarness(t, 5, "DUP_USDT_PERP")
	ctx := context.Background()
	h.seedRunningRealBot(t, "DUP_USDT_PERP")
	h.cleanupSymbol(t, "DUP_USDT_PERP")
	scanID := h.seedAcceptedCandidate(t, "DUP_USDT_PERP")

	if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
		t.Fatalf("deployReal: %v", err)
	}
	decision, reason := h.candidateRow(t, scanID, "DUP_USDT_PERP")
	if decision != "REJECTED" {
		t.Fatalf("occupied symbol must reject the candidate, got %s", decision)
	}
	if !strings.Contains(reason, "символ уже в работе: RUNNING-грид по DUP_USDT_PERP") {
		t.Fatalf("rejection must name the occupied symbol, got %q", reason)
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE account_id = $1 AND symbol = 'DUP_USDT_PERP'
	`, h.account.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the seeded bot must remain the only row, got %d (%v)", rows, err)
	}
	if got := h.mock.createCalls.Load(); got != 0 {
		t.Fatalf("exchange create must not be called for an occupied symbol, got %d", got)
	}
}

// (e) The exchange's symbol-operation refusal (HTTP 403 + a forbidden
// fragment in the body) must defer the candidate with a visible rejection
// instead of leaving it ACCEPTED to re-fail every scan of the refusal
// window. Round 1 pins the pre-flight (checkParams) refusal: rejected
// BEFORE any grid row exists and without touching the create endpoint.
// Round 2 pins the create-stage refusal (check passed, create 403): the
// lifecycle's FAILED row is the single authoritative audit of the refused
// attempt and the candidate still gets the refusal reason. Rounds 3-5 pin
// v2.0.74's broadened classifier: a truncated "Operation is forbidden"
// envelope at checkParams, an unparseable {"data":null,"code":40...} body
// at create, and — negatively — a 403 about the API key which must NOT be
// treated as a symbol state (old behavior: warn and attempt the create).
func TestDeployRealSymbolMaintenanceRejection(t *testing.T) {
	h := newRealDeployHarness(t, 5, "MAINT1_USDT_PERP", "MAINT2_USDT_PERP",
		"MAINT3_USDT_PERP", "MAINT4_USDT_PERP", "MAINT5_USDT_PERP")
	ctx := context.Background()
	for _, symbol := range []string{"MAINT1_USDT_PERP", "MAINT2_USDT_PERP",
		"MAINT3_USDT_PERP", "MAINT4_USDT_PERP", "MAINT5_USDT_PERP"} {
		h.cleanupSymbol(t, symbol)
	}

	// Round 1: checkParams refuses with maintenance — no row, no create.
	h.mock.checkMaint.Store(true)
	scanID := h.seedAcceptedCandidate(t, "MAINT1_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
		t.Fatalf("deployReal round 1: %v", err)
	}
	decision, reason := h.candidateRow(t, scanID, "MAINT1_USDT_PERP")
	if decision != "REJECTED" || !strings.Contains(reason, "биржа запрещает операцию по символу (forbidden/maintenance)") {
		t.Fatalf("check-stage refusal must reject with the forbidden reason, got %s / %q", decision, reason)
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE account_id = $1 AND symbol = 'MAINT1_USDT_PERP'
	`, h.account.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("check-stage refusal must leave no grid row, got %d (%v)", rows, err)
	}
	if got := h.mock.createCalls.Load(); got != 0 {
		t.Fatalf("check-stage refusal must not reach the create endpoint, got %d calls", got)
	}

	// Round 2: checkParams passes, create refuses with maintenance — one
	// FAILED row (the authoritative audit), candidate rejected.
	h.mock.checkMaint.Store(false)
	h.mock.createMaint.Store(true)
	scanID2 := h.seedAcceptedCandidate(t, "MAINT2_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID2, false); err != nil {
		t.Fatalf("deployReal round 2: %v", err)
	}
	decision2, reason2 := h.candidateRow(t, scanID2, "MAINT2_USDT_PERP")
	if decision2 != "REJECTED" || !strings.Contains(reason2, "биржа запрещает операцию по символу (forbidden/maintenance)") {
		t.Fatalf("create-stage refusal must reject with the forbidden reason, got %s / %q", decision2, reason2)
	}
	var status, reconciliation, lastError string
	if err := h.pool.QueryRow(ctx, `
		SELECT status, COALESCE(reconciliation_state, ''), COALESCE(last_error, '')
		FROM grid_bots WHERE account_id = $1 AND symbol = 'MAINT2_USDT_PERP'
	`, h.account.ID).Scan(&status, &reconciliation, &lastError); err != nil {
		t.Fatalf("the refused create must persist its audit row: %v", err)
	}
	if status != "FAILED" || !strings.Contains(lastError, pionexSymbolMaintenanceReason) {
		t.Fatalf("audit row must be FAILED with the exchange reason, got %s / %q", status, lastError)
	}
	if got := h.mock.createCalls.Load(); got != 1 {
		t.Fatalf("create must have been attempted exactly once, got %d", got)
	}

	// Round 3: truncated "Operation is forbidden" envelope at checkParams —
	// parseable body, result=false, no code: the message alone must classify.
	h.mock.createMaint.Store(false)
	h.mock.checkBody.Store(`{"result":false,"message":"Operation is forbidden"}`)
	scanID3 := h.seedAcceptedCandidate(t, "MAINT3_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID3, false); err != nil {
		t.Fatalf("deployReal round 3: %v", err)
	}
	decision3, reason3 := h.candidateRow(t, scanID3, "MAINT3_USDT_PERP")
	if decision3 != "REJECTED" || !strings.Contains(reason3, "forbidden/maintenance") {
		t.Fatalf("truncated forbidden envelope must reject the candidate, got %s / %q", decision3, reason3)
	}

	// Round 4: unparseable {"data":null,"code":40...} body at create — the
	// numeric code breaks envelope decoding, the reason rides the snippet.
	h.mock.checkBody.Store("")
	h.mock.createBody.Store(`{"data":null,"code":403,"reason":"P_TRADING_BOT_OPERATION_IS_FORBIDDEN"}`)
	scanID4 := h.seedAcceptedCandidate(t, "MAINT4_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID4, false); err != nil {
		t.Fatalf("deployReal round 4: %v", err)
	}
	decision4, reason4 := h.candidateRow(t, scanID4, "MAINT4_USDT_PERP")
	if decision4 != "REJECTED" || !strings.Contains(reason4, "forbidden/maintenance") {
		t.Fatalf("snippet-only forbidden body must reject the candidate, got %s / %q", decision4, reason4)
	}

	// Round 5 (negative): a 403 about the API key is a credential refusal,
	// never a symbol state — the deploy path must attempt the create and the
	// candidate must stay ACCEPTED until a real outcome resolves it.
	h.mock.createBody.Store(`{"result":false,"code":"P_API_KEY_INVALID","message":"api key is invalid"}`)
	scanID5 := h.seedAcceptedCandidate(t, "MAINT5_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID5, false); err != nil {
		t.Fatalf("deployReal round 5: %v", err)
	}
	decision5, _ := h.candidateRow(t, scanID5, "MAINT5_USDT_PERP")
	if decision5 == "REJECTED" {
		t.Fatalf("credential 403 must not be classified as a symbol refusal")
	}
	if got := h.mock.createCalls.Load(); got < 2 {
		t.Fatalf("credential 403 must fall through to a create attempt, got %d creates", got)
	}
}
