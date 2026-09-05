package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// v2.0.93 parity release tests. One file, one harness style per arm:
//   FIX-D  manual PAPER deploy through the durable risk engine
//   FIX-E  REAL tranche-2 pour waits for a calm (HOLD) tick
//   FIX-F  paper LLM-audit gate (REAL parity)
//   FIX-G  REAL sector cap + DOM depth gate (paper parity)
//   FIX-J  paper range shift in keep_investment / normal modes
//   FIX-B  settings update preserves the omitted switches
//   FIX-A  validateSettings fee/slippage sanity

// ---------------------------------------------------------------------------
// FIX-D: manual PAPER deploys run the durable risk exam (AGENTS.md #5).
// ---------------------------------------------------------------------------

// TestV2093ManualPaperDeployRiskGates pins the v2.0.93 FIX-D wiring: the
// manual PAPER branch of DeployManualBot used to skip the risk engine
// entirely — no kill switch, no MaxLeverage, no exposure caps — while the
// manual REAL branch and every scan deploy ran them.
func TestV2093ManualPaperDeployRiskGates(t *testing.T) {
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

	var savedKill bool
	var savedMaxLev int
	if err := pool.QueryRow(ctx, `
		SELECT kill_switch_enabled, max_leverage FROM risk_settings WHERE id = 1
	`).Scan(&savedKill, &savedMaxLev); err != nil {
		t.Fatalf("snapshot risk: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE risk_settings SET kill_switch_enabled = $2, max_leverage = $3 WHERE id = 1`,
			savedKill, savedMaxLev)
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'V93D%_USDT_PERP'`)
	})

	mock := newManualDeployExchangeMock(t, "30")
	service.publicAPI = pionex.NewClient(mock.server.URL, "", "")

	deploy := func(symbol string, leverage int) error {
		t.Helper()
		_, _, deployErr := service.DeployManualBot(ctx, nil, ManualDeployInput{
			Symbol: symbol, Mode: "PAPER", Direction: "NEUTRAL", Leverage: leverage,
			Lower: decimal.NewFromInt(98), Upper: decimal.NewFromInt(102), Row: 12,
		})
		return deployErr
	}

	// Kill switch ON: refused — the exact AGENTS.md #5 violation class.
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings SET kill_switch_enabled = true, max_leverage = 10 WHERE id = 1
	`); err != nil {
		t.Fatalf("arm kill switch: %v", err)
	}
	if err := deploy("V93D1_USDT_PERP", 2); err == nil || !strings.Contains(err.Error(), "kill switch") {
		t.Fatalf("manual PAPER deploy must be refused while the kill switch is armed, got %v", err)
	}

	// Kill switch OFF but the leverage over the durable cap: still refused.
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings SET kill_switch_enabled = false WHERE id = 1
	`); err != nil {
		t.Fatalf("disarm kill switch: %v", err)
	}
	if err := deploy("V93D2_USDT_PERP", 50); err == nil || !strings.Contains(err.Error(), "leverage") {
		t.Fatalf("manual PAPER deploy must be refused above the durable MaxLeverage, got %v", err)
	}

	// Nothing was written for any refused deploy.
	var refused int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol LIKE 'V93D%_USDT_PERP'
	`).Scan(&refused); err != nil || refused != 0 {
		t.Fatalf("refused manual deploys must leave no paper rows, got %d (%v)", refused, err)
	}

	// Clean config deploys — the gates protect without false-blocking.
	if err := deploy("V93D3_USDT_PERP", 2); err != nil {
		t.Fatalf("clean manual PAPER deploy must pass the risk exam, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// FIX-E: the REAL tranche-2 pour runs AFTER the stop decision.
// ---------------------------------------------------------------------------

// TestV2093RealTranchePourWaitsForHoldTick pins the paper-canonical order:
// on a conflicting tick (stop decision + armed 24h time-box top-up) the stop
// wins and the pour waits. The old order poured real margin first and closed
// the bot on the very same tick.
func TestV2093RealTranchePourWaitsForHoldTick(t *testing.T) {
	const symbol = "TRHOLD_USDT_PERP"
	// Price 80 is far below the seeded 90..110 range: the manage pass must
	// decide RANGE_BREAK_DOWN (unknown regime → adverse) and close. The mock
	// investment stays at the tranche-1 level (no self-heal interference).
	mock := newTrancheSelfHealMock(t, symbol, "80", "100")
	worker, pool, settingsID := trancheSelfHealFixture(t, mock.server.URL)
	ctx := context.Background()

	var accountID string
	if err := pool.QueryRow(ctx, `
		SELECT account_id FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&accountID); err != nil {
		t.Fatalf("resolve fixture account: %v", err)
	}

	// 25h old tranche-1 bot: the 24h time-box top-up is armed, and the
	// 1-candle klines stub means "not trending" — the old order poured here.
	botID := seedTrancheBot(t, pool, accountID, settingsID, symbol, "TRHOLD-1",
		decimal.NewFromInt(100), nil)

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	if got := mock.investCount(); got != 0 {
		t.Fatalf("a stop-decision tick must not pour real margin, got %d invest_in calls", got)
	}
	var status, reason string
	var trancheDeployed int
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason, ''), COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0)
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&status, &reason, &trancheDeployed); err != nil {
		t.Fatalf("load bot after manage: %v", err)
	}
	// The mock has no cancel endpoint, so the native cancel maps the 404 to
	// "already closed" and settles the row STOPPED/ALREADY_CLOSED — either
	// terminal is the stop decision executing; the invariant under test is
	// that the bot left RUNNING by CLOSE, not by pour.
	if status != "STOP_REQUESTED" && !(status == "STOPPED" && reason == "ALREADY_CLOSED") {
		t.Fatalf("the stop decision must win the conflicting tick, got status=%s reason=%s", status, reason)
	}
	if trancheDeployed != 1 {
		t.Fatalf("the pour must wait for a calm tick (trancheDeployed stays 1), got %d", trancheDeployed)
	}
	var trancheEvents int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events WHERE bot_id = $1::TEXT AND event_type = 'TRANCHE_2'
	`, botID).Scan(&trancheEvents); err != nil || trancheEvents != 0 {
		t.Fatalf("no TRANCHE_2 event may fire on a stop tick, got %d (%v)", trancheEvents, err)
	}
}

// ---------------------------------------------------------------------------
// FIX-F: the paper deploy runs the LLM-audit gate the REAL arm runs.
// ---------------------------------------------------------------------------

// TestV2093DeployPaperLLMAuditGate pins parity: with the LLM brain enabled,
// an unaudited candidate is not deployable in PAPER either; an audited one
// deploys. Fail-open holds when the brain is disabled (the default state
// every other paper test relies on).
func TestV2093DeployPaperLLMAuditGate(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, _, settings := newCooldownTestWorker(t, pool)
	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "V93LLM_USDT_PERP", "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	const symbol = "V93LLM_USDT_PERP"
	var savedLLMEnabled, savedLLMKey string
	if err := pool.QueryRow(ctx, `
		SELECT enabled::TEXT, COALESCE(api_key, '') FROM llm_settings WHERE id = 1
	`).Scan(&savedLLMEnabled, &savedLLMKey); err != nil {
		t.Fatalf("snapshot llm settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE llm_settings SET enabled = $2::BOOLEAN, api_key = $3 WHERE id = 1`,
			savedLLMEnabled == "true", savedLLMKey)
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol)
		_, _ = pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol = $1`, symbol)
		_, _ = pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id IN (
			SELECT scan_id FROM autogrid_candidates WHERE symbol = $1)`, symbol)
		_, _ = pool.Exec(ctx, `DELETE FROM autogrid_candidates WHERE symbol = $1`, symbol)
	})

	seedScan := func(withAudit bool) string {
		t.Helper()
		tradedTF := normalizeBacktestTF(settings.CandleInterval)
		for _, tf := range append([]string{tradedTF}, neighborBacktestTFs(tradedTF)...) {
			if _, err := pool.Exec(ctx, `
				INSERT INTO backtest_jobs (symbol, interval, status, result, finished_at)
				VALUES ($1, $2, 'DONE',
				        '{"folds": 4, "oos_return_pct": 1.2, "oos_max_drawdown": 0.05, "round_trips": 100, "stop_hits": 0}'::jsonb,
				        NOW())
			`, symbol, tf); err != nil {
				t.Fatalf("seed backtest job %s: %v", tf, err)
			}
		}
		var scanID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
		`).Scan(&scanID); err != nil {
			t.Fatalf("insert scan run: %v", err)
		}
		assumptions := `{"atrPct": 1.0, "regime": "RANGE"}`
		if withAudit {
			assumptions = `{"atrPct": 1.0, "regime": "RANGE", "llmAuditId": "11111111-1111-1111-1111-111111111111"}`
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO autogrid_candidates (
				scan_id, symbol, decision, current_price, lower_price, upper_price,
				grid_num, recommended_trend, model_assumptions
			) VALUES ($1, $2, 'ACCEPTED', 100, 90, 110, 10, 'no_trend', $3::jsonb)
		`, scanID, symbol, assumptions); err != nil {
			t.Fatalf("insert candidate: %v", err)
		}
		return scanID
	}

	// Brain ON, candidate unaudited → refused with the REAL-arm text.
	if _, err := pool.Exec(ctx, `
		UPDATE llm_settings SET enabled = true, api_key = 'itest-key' WHERE id = 1
	`); err != nil {
		t.Fatalf("enable llm brain: %v", err)
	}
	scanNoAudit := seedScan(false)
	if err := worker.deployPaper(ctx, settings, scanNoAudit, false); err != nil {
		t.Fatalf("deployPaper (unaudited): %v", err)
	}
	var running int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil || running != 0 {
		t.Fatalf("unaudited candidate must not deploy in paper, got %d bots (%v)", running, err)
	}
	var decision, reason string
	if err := pool.QueryRow(ctx, `
		SELECT decision, COALESCE(rejection_reason, '') FROM autogrid_candidates
		WHERE scan_id = $1 AND symbol = $2
	`, scanNoAudit, symbol).Scan(&decision, &reason); err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if decision != "REJECTED" || !strings.Contains(reason, "AI-аудит") {
		t.Fatalf("rejection must carry the LLM-audit text, got %s / %q", decision, reason)
	}

	// Audited candidate → deploys (same brain, same everything else).
	scanAudited := seedScan(true)
	if err := worker.deployPaper(ctx, settings, scanAudited, false); err != nil {
		t.Fatalf("deployPaper (audited): %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil || running != 1 {
		t.Fatalf("audited candidate must deploy, got %d bots (%v)", running, err)
	}

	// Brain OFF → fail-open: the gate disappears for a fresh unaudited scan.
	if _, err := pool.Exec(ctx, `
		UPDATE llm_settings SET enabled = false WHERE id = 1
	`); err != nil {
		t.Fatalf("disable llm brain: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol); err != nil {
		t.Fatalf("clear bots for fail-open round: %v", err)
	}
	scanFailOpen := seedScan(false)
	if err := worker.deployPaper(ctx, settings, scanFailOpen, false); err != nil {
		t.Fatalf("deployPaper (fail-open): %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil || running != 1 {
		t.Fatalf("disabled brain must fail open, got %d bots (%v)", running, err)
	}
}

// ---------------------------------------------------------------------------
// FIX-G: the REAL deploy runs the sector cap and the DOM depth gate.
// ---------------------------------------------------------------------------

// TestV2093DeployRealSectorCap: three same-sector RUNNING grids must block a
// fourth correlated entry — the cap the paper fleet has enforced since
// v2.0.27, missing from deployReal until v2.0.93.
func TestV2093DeployRealSectorCap(t *testing.T) {
	h := newRealDeployHarness(t, 5, "NVDA_USDT_PERP")
	ctx := context.Background()
	h.cleanupSymbol(t, "NVDA_USDT_PERP")
	for _, semis := range []string{"AMDX_USDT_PERP", "MU_USDT_PERP", "TSM_USDT_PERP"} {
		h.seedRunningRealBot(t, semis)
		h.cleanupSymbol(t, semis)
	}

	scanID := h.seedAcceptedCandidate(t, "NVDA_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
		t.Fatalf("deployReal: %v", err)
	}
	decision, reason := h.candidateRow(t, scanID, "NVDA_USDT_PERP")
	if decision != "REJECTED" || !strings.Contains(reason, "sector cap") {
		t.Fatalf("the fourth semis grid must be refused by the sector cap, got %s / %q", decision, reason)
	}
	var nvdaBots int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE symbol = 'NVDA_USDT_PERP'
	`).Scan(&nvdaBots); err != nil || nvdaBots != 0 {
		t.Fatalf("no NVDA grid row may be created, got %d (%v)", nvdaBots, err)
	}
}

// TestV2093DeployRealDepthGate: a one-sided ask book against a NEUTRAL entry
// vetoes the REAL deploy — the DOM gate the paper arm has run since v2.0.39.
func TestV2093DeployRealDepthGate(t *testing.T) {
	h := newRealDeployHarness(t, 5, "DOMV_USDT_PERP")
	ctx := context.Background()
	h.cleanupSymbol(t, "DOMV_USDT_PERP")

	// Front proxy: the harness mock has no depth endpoint, so every public
	// call is proxied through and /api/v1/market/depth gets an ask-heavy
	// book (bids $100 vs asks ~$1010 inside the 1.5% band → imbalance ≈ 0.09
	// < 0.25 → veto against a non-short entry).
	target, _ := url.Parse(h.mock.server.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	depth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/market/depth" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": true, "timestamp": time.Now().UnixMilli(),
				"data": map[string]any{
					"bids": [][2]string{{"100", "1"}},
					"asks": [][2]string{{"100.4", "25"}},
				},
			})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(depth.Close)
	h.worker.publicClient = pionex.NewClient(depth.URL, "", "")

	scanID := h.seedAcceptedCandidate(t, "DOMV_USDT_PERP")
	if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
		t.Fatalf("deployReal: %v", err)
	}
	decision, reason := h.candidateRow(t, scanID, "DOMV_USDT_PERP")
	if decision != "REJECTED" || !strings.Contains(reason, "стакан") {
		t.Fatalf("an ask-dominated book must veto the REAL deploy, got %s / %q", decision, reason)
	}
	var bots int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE symbol = 'DOMV_USDT_PERP'
	`).Scan(&bots); err != nil || bots != 0 {
		t.Fatalf("no grid row may be created, got %d (%v)", bots, err)
	}
}

// ---------------------------------------------------------------------------
// FIX-J: the paper range shift ships keep_investment semantics under water.
// ---------------------------------------------------------------------------

// TestV2093PaperRangeShiftModes exercises both shift arms of the paper manage
// loop against the REAL path's adjustShiftMode preflight:
//   - under water → keep_investment: the floating loss CARRIES as unrealized
//     against the new bounds (no crystallization, entry basis untouched);
//   - green (≥ +$0.10) → normal re-base: the mark crystallizes into realized,
//     unrealized resets, a directional entry re-anchors.
func TestV2093PaperRangeShiftModes(t *testing.T) {
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
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	// Flat 60-candle klines → DetectRegime = RANGE, so a LONG down-break
	// shifts (not closes) and the 24h self-baselines stay deterministic.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": []map[string]any{
				{"symbol": "V93KEEP_USDT_PERP", "close": "80", "open": "100",
					"high": "101", "low": "79", "volume": "1000"},
				{"symbol": "V93NORM_USDT_PERP", "close": "120", "open": "100",
					"high": "121", "low": "99", "volume": "1000"},
			}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, _ *http.Request) {
		klines := make([]map[string]any, 0, 60)
		for i := 0; i < 60; i++ {
			klines = append(klines, map[string]any{
				"time": time.Now().UnixMilli() - int64(60-i)*600_000,
				"open": "100", "close": "100", "high": "101", "low": "99", "volume": "10",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": klines},
		})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)

	var savedRadar, savedDgt string
	if err := pool.QueryRow(ctx, `
		SELECT stop_forecast_mode, dgt_redeploy_enabled::TEXT FROM autogrid_settings WHERE id = $1
	`, settings.ID).Scan(&savedRadar, &savedDgt); err != nil {
		t.Fatalf("snapshot modes: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET stop_forecast_mode = $2, dgt_redeploy_enabled = $3::BOOLEAN WHERE id = $1`,
			settings.ID, savedRadar, savedDgt == "true")
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol IN ('V93KEEP_USDT_PERP','V93NORM_USDT_PERP')`)
		_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol IN ('V93KEEP_USDT_PERP','V93NORM_USDT_PERP')`)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings SET stop_forecast_mode = 'OFF', dgt_redeploy_enabled = false WHERE id = $1
	`, settings.ID); err != nil {
		t.Fatalf("pin modes: %v", err)
	}

	seedLongBot := func(symbol string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, realized_pnl_usdt, pnl_target_usdt, max_loss_usdt
			) VALUES (
				$1, $2, 'RUNNING', 'LONG', 'ARITHMETIC',
				90, 110, 10, 2, 200,
				100, 100, 1, 999, -999
			)
			RETURNING id
		`, settings.ID, symbol).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", symbol, err)
		}
		return id
	}
	keepID := seedLongBot("V93KEEP_USDT_PERP")
	normID := seedLongBot("V93NORM_USDT_PERP")

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(stub.URL, "test-key", "test-secret")

	if err := worker.managePaperBots(ctx, *settings); err != nil {
		t.Fatalf("managePaperBots: %v", err)
	}

	// Keep arm: price 80 → deeply negative floating PnL → the shift must
	// carry the loss, not crystallize it.
	var keepStatus string
	var keepRealized, keepUnrealized, keepEntry decimal.Decimal
	var keepAdjustments int
	if err := pool.QueryRow(ctx, `
		SELECT status, realized_pnl_usdt, unrealized_pnl_usdt, entry_price, adjustments_count
		FROM paper_grid_bots WHERE id = $1
	`, keepID).Scan(&keepStatus, &keepRealized, &keepUnrealized, &keepEntry, &keepAdjustments); err != nil {
		t.Fatalf("load keep bot: %v", err)
	}
	if keepStatus != "RUNNING" {
		t.Fatalf("keep bot must stay RUNNING after the rescue shift, got %s", keepStatus)
	}
	if keepAdjustments != 1 {
		t.Fatalf("the shift must consume one adjustment, got %d", keepAdjustments)
	}
	if !keepRealized.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("keep_investment must NOT crystallize the floating loss (realized stays 1), got %s", keepRealized)
	}
	if !(keepUnrealized.LessThan(decimal.NewFromInt(-60))) {
		t.Fatalf("the floating loss must carry forward as unrealized, got %s", keepUnrealized)
	}
	if !keepEntry.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("keep_investment must keep the position's entry basis (100), got %s", keepEntry)
	}
	keepMode := shiftEventMode(t, pool, keepID)
	if keepMode != "keep_investment" {
		t.Fatalf("ADJUST_RANGE event must record mode=keep_investment, got %q", keepMode)
	}

	// Normal arm: price 120 → strongly positive floating PnL → re-base.
	var normStatus string
	var normRealized, normUnrealized, normEntry decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT status, realized_pnl_usdt, unrealized_pnl_usdt, entry_price
		FROM paper_grid_bots WHERE id = $1
	`, normID).Scan(&normStatus, &normRealized, &normUnrealized, &normEntry); err != nil {
		t.Fatalf("load normal bot: %v", err)
	}
	if normStatus != "RUNNING" {
		t.Fatalf("normal bot must stay RUNNING after the shift, got %s", normStatus)
	}
	if !(normRealized.GreaterThan(decimal.NewFromInt(70))) {
		t.Fatalf("normal re-base must crystallize the green mark into realized, got %s", normRealized)
	}
	if !normUnrealized.IsZero() {
		t.Fatalf("normal re-base resets unrealized, got %s", normUnrealized)
	}
	if !normEntry.Equal(decimal.NewFromInt(120)) {
		t.Fatalf("normal re-base re-anchors the directional entry to 120, got %s", normEntry)
	}
	normMode := shiftEventMode(t, pool, normID)
	if normMode != "normal" {
		t.Fatalf("ADJUST_RANGE event must record mode=normal, got %q", normMode)
	}
}

func shiftEventMode(t *testing.T, pool *pgxpool.Pool, botID string) string {
	t.Helper()
	var mode *string
	if err := pool.QueryRow(context.Background(), `
		SELECT details->>'mode' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE'
		ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&mode); err != nil {
		t.Fatalf("load ADJUST_RANGE event for %s: %v", botID, err)
	}
	if mode == nil {
		return ""
	}
	return *mode
}

// ---------------------------------------------------------------------------
// FIX-B: a partial settings update preserves the omitted switches.
// ---------------------------------------------------------------------------

// TestV2093UpdateSettingsPreservesSwitches pins the preserve-on-omit contract
// the MCP layer now relies on: scanMode, the autotune interval, tranche and
// DGT switches survive an update that does not mention them (the bool fields
// the MCP handler backfills from current are exercised end-to-end here at
// the service boundary).
func TestV2093UpdateSettingsPreservesSwitches(t *testing.T) {
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

	// A distinctive stored state: FULL scans, autotune ON at a non-default
	// cadence, tranche OFF (operator disarmed), DGT ON.
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET scan_mode = 'FULL', ai_autotune_enabled = true,
		    ai_autotune_interval_seconds = 1800, tranche_deploy_enabled = false,
		    dgt_redeploy_enabled = true, ai_kit_enabled = true
		WHERE id = $1
	`, settings.ID); err != nil {
		t.Fatalf("seed distinctive switches: %v", err)
	}
	current, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}

	// A partial update that mentions NONE of the switches (the shape a
	// partial MCP form produces after the v2.0.93 backfill).
	update := UpdateSettingsInput{
		AccountID:               current.AccountID,
		ExecutionMode:           current.ExecutionMode,
		BudgetUSDT:              current.BudgetUSDT,
		MaxActiveBots:           current.MaxActiveBots,
		Leverage:                current.Leverage,
		MinSharpe:               current.MinSharpe,
		MinEVPct:                current.MinEVPct,
		StopLossMode:            current.StopLossMode,
		SmartPNLEnabled:         current.SmartPNLEnabled,
		AdaptiveLeverageEnabled: current.AdaptiveLeverageEnabled,
		DensityGridEnabled:      current.DensityGridEnabled,
		CandleInterval:          current.CandleInterval,
		LookbackCandles:         current.LookbackCandles,
		MaxSymbolsPerScan:       current.MaxSymbolsPerScan,
		ScanIntervalSeconds:     current.ScanIntervalSeconds,
		MinVolume24h:            current.MinVolume24h,
		MinVolatilityPct:        current.MinVolatilityPct,
		MaxVolatilityPct:        current.MaxVolatilityPct,
		MaxDrawdownPct:          current.MaxDrawdownPct,
		MinProfitFactor:         current.MinProfitFactor,
		FeeBps:                  current.FeeBps,
		SlippageBps:             current.SlippageBps,
		PnLTargetUSDT:           current.PnLTargetUSDT,
		MaxLossUSDT:             current.MaxLossUSDT,
		ManageIntervalSeconds:   current.ManageIntervalSeconds,
		RangeBreakBufferPct:     current.RangeBreakBufferPct,
		MaxAdjustmentsPerBot:    current.MaxAdjustmentsPerBot,
		// The MCP backfill contract: non-pointer bools carry the CURRENT
		// value when the operator omitted the field.
		AIKitEnabled:      current.AIKitEnabled,
		AIAutotuneEnabled: current.AIAutotuneEnabled,
		// ScanMode "" / AIAutotuneInterval 0 / nil pointers = omitted.
	}
	updated, err := service.UpdateSettings(ctx, update)
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	if updated.ScanMode != "FULL" {
		t.Fatalf("omitted scanMode must stay FULL, got %s", updated.ScanMode)
	}
	if !updated.AIAutotuneEnabled {
		t.Fatal("omitted aiAutotuneEnabled must stay true — the v2.0.93 FIX-B autotune kill")
	}
	if updated.AIAutotuneInterval != 1800 {
		t.Fatalf("omitted autotune interval must stay 1800, got %d", updated.AIAutotuneInterval)
	}
	if updated.TrancheDeployEnabled {
		t.Fatal("omitted trancheDeployEnabled must stay false (operator had disarmed it)")
	}
	if !updated.DgtRedeployEnabled {
		t.Fatal("omitted dgtRedeployEnabled must stay true")
	}

	// An EXPLICIT value still lands (preserve-on-omit must not become
	// ignore-on-provide).
	enabled := true
	update.TrancheDeployEnabled = &enabled
	update.ScanMode = "TOP_K"
	updated, err = service.UpdateSettings(ctx, update)
	if err != nil {
		t.Fatalf("explicit update: %v", err)
	}
	if !updated.TrancheDeployEnabled || updated.ScanMode != "TOP_K" {
		t.Fatalf("explicit values must land, got tranche=%v scan=%s",
			updated.TrancheDeployEnabled, updated.ScanMode)
	}
}

// ---------------------------------------------------------------------------
// FIX-A: validateSettings guards the fee/slippage pair against typos.
// ---------------------------------------------------------------------------

// TestV2093ValidateSettingsFeeSlippageSanity: feeBps outside [0.5..50] or
// slippageBps outside [0..30] must fail validation with a named error — the
// pair now drives BOTH the fee-gate and the density floor, so a typo (0.5
// instead of 5) starves or floods the whole fleet silently.
func TestV2093ValidateSettingsFeeSlippageSanity(t *testing.T) {
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

	var savedKill bool
	var savedSymCap, savedAcctCap, savedDaily string
	if err := pool.QueryRow(ctx, `
		SELECT kill_switch_enabled, max_symbol_exposure_usd::TEXT,
		       max_account_exposure_usd::TEXT, max_daily_loss_usd::TEXT
		FROM risk_settings WHERE id = 1
	`).Scan(&savedKill, &savedSymCap, &savedAcctCap, &savedDaily); err != nil {
		t.Fatalf("snapshot risk: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE risk_settings SET kill_switch_enabled = $2,
			max_symbol_exposure_usd = $3::NUMERIC, max_account_exposure_usd = $4::NUMERIC,
			max_daily_loss_usd = $5::NUMERIC WHERE id = 1`,
			savedKill, savedSymCap, savedAcctCap, savedDaily)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings SET kill_switch_enabled = false,
		    max_symbol_exposure_usd = 100000, max_account_exposure_usd = 100000,
		    max_daily_loss_usd = 100000, max_leverage = 10, max_active_grid_bots = 10
		WHERE id = 1
	`); err != nil {
		t.Fatalf("pin risk caps: %v", err)
	}

	base := UpdateSettingsInput{
		AccountID:               nil,
		ExecutionMode:           "PAPER",
		BudgetUSDT:              settings.BudgetUSDT,
		MaxActiveBots:           settings.MaxActiveBots,
		Leverage:                settings.Leverage,
		MinSharpe:               settings.MinSharpe,
		MinEVPct:                settings.MinEVPct,
		StopLossMode:            settings.StopLossMode,
		SmartPNLEnabled:         settings.SmartPNLEnabled,
		AdaptiveLeverageEnabled: settings.AdaptiveLeverageEnabled,
		DensityGridEnabled:      settings.DensityGridEnabled,
		CandleInterval:          settings.CandleInterval,
		LookbackCandles:         settings.LookbackCandles,
		MaxSymbolsPerScan:       settings.MaxSymbolsPerScan,
		ScanIntervalSeconds:     settings.ScanIntervalSeconds,
		MinVolume24h:            settings.MinVolume24h,
		MinVolatilityPct:        settings.MinVolatilityPct,
		MaxVolatilityPct:        settings.MaxVolatilityPct,
		MaxDrawdownPct:          settings.MaxDrawdownPct,
		MinProfitFactor:         settings.MinProfitFactor,
		FeeBps:                  decimal.NewFromInt(5),
		SlippageBps:             decimal.NewFromInt(2),
		PnLTargetMode:           settings.PnLTargetMode,
		PnLTargetUSDT:           settings.PnLTargetUSDT,
		MaxLossUSDT:             settings.MaxLossUSDT,
		ManageIntervalSeconds:   settings.ManageIntervalSeconds,
		RangeBreakBufferPct:     settings.RangeBreakBufferPct,
		MaxAdjustmentsPerBot:    settings.MaxAdjustmentsPerBot,
		AIAutotuneInterval:      settings.AIAutotuneInterval,
	}
	if err := service.validateSettings(ctx, base); err != nil {
		t.Fatalf("baseline 5/2 bps must validate, got %v", err)
	}

	for _, tc := range []struct {
		name                string
		feeBps, slippageBps float64
		fragment            string
	}{
		{"fee typo low", 0.4, 2, "feeBps"},
		{"fee absurd high", 51, 2, "feeBps"},
		{"slippage absurd high", 5, 31, "slippageBps"},
	} {
		input := base
		input.FeeBps = decimal.NewFromFloat(tc.feeBps)
		input.SlippageBps = decimal.NewFromFloat(tc.slippageBps)
		err := service.validateSettings(ctx, input)
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Fatalf("%s (%v/%v bps) must fail validation naming %q, got %v",
				tc.name, tc.feeBps, tc.slippageBps, tc.fragment, err)
		}
	}
	// Boundaries are legal: 0.5..50 fee, 0..30 slippage.
	for _, pair := range [][2]float64{{0.5, 0}, {50, 30}} {
		input := base
		input.FeeBps = decimal.NewFromFloat(pair[0])
		input.SlippageBps = decimal.NewFromFloat(pair[1])
		if err := service.validateSettings(ctx, input); err != nil {
			t.Fatalf("boundary %v/%v bps must validate, got %v", pair[0], pair[1], err)
		}
	}
}
