package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// gridLifecycleExchangeMock serves every endpoint the manage loops and the
// DGT re-deploy touch for BOTH fleets:
//
//   - public tickers (priceMap fallback — /market/indexes stays unmocked and
//     404s, which is the documented outage path),
//   - klines, switched by interval: the OU tape on the scanner cadence (15M),
//     empty answers for the regime (60M) and HAR (1D) fetches so the regime
//     reads UNKNOWN and the geometry falls back to the ATR stub,
//   - the native futures-grid endpoints: order detail (running → finished,
//     flipped by the test between passes), cancel, checkParams, create and
//     the common symbols list.
type gridLifecycleExchangeMock struct {
	server        *httptest.Server
	mu            sync.Mutex
	prices        map[string]string
	orderFinished atomic.Bool
	cancelCount   atomic.Int64
	createCount   atomic.Int64
	klinesSymbols map[string]bool // symbols serving the OU tape on the 15M cadence
	lastCreate    map[string]any
}

func newGridLifecycleExchangeMock(t *testing.T, prices map[string]string) *gridLifecycleExchangeMock {
	t.Helper()
	mock := &gridLifecycleExchangeMock{prices: prices}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		tickers := make([]map[string]any, 0, len(prices))
		for symbol, price := range prices {
			tickers = append(tickers, map[string]any{
				"symbol": symbol, "close": price, "open": price,
				"high": price, "low": price, "volume": "1000",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": tickers},
		})
	})

	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, r *http.Request) {
		interval := r.URL.Query().Get("interval")
		symbol := r.URL.Query().Get("symbol")
		var candles []map[string]any
		mock.mu.Lock()
		serveOU := interval == "15M" && mock.klinesSymbols[symbol]
		mock.mu.Unlock()
		if serveOU {
			// Deterministic AR(1) around 1000 with b = 2^(-1/16)-1: HL is
			// exactly 16 steps → 4 hours on the 15m cadence → max age 8h.
			b := 0.9575952178380853 - 1 // 2^(-1/16)
			p := 1100.0
			for i := 0; i < 192; i++ {
				p = p + b*(p-1000)
				candles = append(candles, map[string]any{
					"time":   time.Now().UnixMilli() - int64(i)*15*60*1000,
					"open":   strconv.FormatFloat(p, 'f', 6, 64),
					"close":  strconv.FormatFloat(p, 'f', 6, 64),
					"high":   strconv.FormatFloat(p+1, 'f', 6, 64),
					"low":    strconv.FormatFloat(p-1, 'f', 6, 64),
					"volume": "10",
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": candles},
		})
	})

	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		status := "running"
		reasonBy := ""
		if mock.orderFinished.Load() {
			status = "finished"
			reasonBy = "user_cancel"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    status, "reasonBy": reasonBy,
				"buOrderData": map[string]any{
					"status": status, "top": "110", "bottom": "90", "row": "20",
					"position": "0", "gridProfit": "0.5",
				},
			},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.cancelCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/checkParams", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"min_investment": "5", "slippage": "0.1"},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/create", func(w http.ResponseWriter, r *http.Request) {
		mock.createCount.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.mu.Lock()
		mock.lastCreate = body
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"buOrderId": fmt.Sprintf("DGT-%d", mock.createCount.Load())},
		})
	})
	mux.HandleFunc("GET /api/v1/common/symbols", func(w http.ResponseWriter, _ *http.Request) {
		symbols := make([]map[string]any, 0, len(prices))
		for symbol := range prices {
			symbols = append(symbols, map[string]any{
				"symbol": symbol, "type": "PERP", "enable": true, "status": "TRADING",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"symbols": symbols},
		})
	})

	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *gridLifecycleExchangeMock) create() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]any, len(m.lastCreate))
	for k, v := range m.lastCreate {
		cp[k] = v
	}
	return cp
}

// setOUTape arms the mean-reverting kline tape for a symbol.
func (m *gridLifecycleExchangeMock) setOUTape(symbol string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.klinesSymbols == nil {
		m.klinesSymbols = make(map[string]bool)
	}
	m.klinesSymbols[symbol] = true
}

// gridLifecyclePaperHarness wires a Worker + Service against the disposable
// DB and the exchange mock, with save/restore of the shared settings row
// (the scope is a singleton — tests must not leak mutations).
type gridLifecyclePaperHarness struct {
	pool     *pgxpool.Pool
	worker   *Worker
	service  *Service
	mock     *gridLifecycleExchangeMock
	settings *Settings
}

func newGridLifecyclePaperHarness(t *testing.T, prices map[string]string) *gridLifecyclePaperHarness {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newGridLifecycleExchangeMock(t, prices)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(mock.server.URL, "test-key", "test-secret")

	return &gridLifecyclePaperHarness{
		pool: pool, worker: worker, service: service, mock: mock, settings: settings,
	}
}

// patchSettings applies SQL assignments to the shared settings row and
// registers a cleanup restoring the snapshot taken before the patch.
func (h *gridLifecyclePaperHarness) patchSettings(t *testing.T, setClause string, args ...any) {
	t.Helper()
	ctx := context.Background()
	var saved []string
	rows, err := h.pool.Query(ctx, `
		SELECT dgt_redeploy_enabled::TEXT, tranche_deploy_enabled::TEXT
		FROM autogrid_settings WHERE id = $1
	`, h.settings.ID)
	if err != nil {
		t.Fatalf("snapshot settings: %v", err)
	}
	if rows.Next() {
		var dgt, tranche string
		if scanErr := rows.Scan(&dgt, &tranche); scanErr == nil {
			saved = []string{dgt, tranche}
		}
	}
	rows.Close()

	args = append([]any{h.settings.ID}, args...)
	if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE autogrid_settings SET %s, updated_at = NOW() WHERE id = $1
	`, setClause), args...); err != nil {
		t.Fatalf("patch settings (%s): %v", setClause, err)
	}
	t.Cleanup(func() {
		if saved == nil {
			return
		}
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE autogrid_settings
			SET dgt_redeploy_enabled = $2::BOOLEAN, tranche_deploy_enabled = $3::BOOLEAN
			WHERE id = $1
		`, h.settings.ID, saved[0], saved[1])
	})
}

func (h *gridLifecyclePaperHarness) reloadSettings(t *testing.T) Settings {
	t.Helper()
	settings, err := h.service.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	return *settings
}

// seedPaperBot inserts one RUNNING paper bot and returns its id.
func (h *gridLifecyclePaperHarness) seedPaperBot(
	t *testing.T, symbol, direction string, adjustments int, openedAt string,
) string {
	t.Helper()
	var botID string
	err := h.pool.QueryRow(context.Background(), `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt,
			adjustments_count, opened_at
		) VALUES (
			$1, $2, 'RUNNING', $3, 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 999, -999,
			$4, NOW() - $5::INTERVAL
		)
		RETURNING id
	`, h.settings.ID, symbol, direction, adjustments, openedAt).Scan(&botID)
	if err != nil {
		t.Fatalf("seed paper bot: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := h.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup paper bots: %v", err)
		}
	})
	return botID
}

// TestPaperDgtRedeploysOnUpBreak pins the paper arm of FIX-1: a SHORT bot
// whose range breaks UP with the shift budget exhausted closes with
// RANGE_BREAK_UP AND a fresh RUNNING grid appears in the SAME manage pass —
// same symbol, same direction, same slot budget, center at the break price —
// with a DGT_REDEPLOY event on the new row.
func TestPaperDgtRedeploysOnUpBreak(t *testing.T) {
	const symbol = "DGTU_USDT_PERP"
	h := newGridLifecyclePaperHarness(t, map[string]string{symbol: "130"})
	h.patchSettings(t, "dgt_redeploy_enabled = TRUE, tranche_deploy_enabled = FALSE")
	seeded := h.seedPaperBot(t, symbol, "SHORT", 3, "0 minutes")

	if err := h.worker.managePaperBots(context.Background(), h.reloadSettings(t)); err != nil {
		t.Fatalf("managePaperBots: %v", err)
	}

	var closedReason, newID string
	var newLower, newUpper, newInvestment decimal.Decimal
	if err := h.pool.QueryRow(context.Background(), `
		SELECT closed_reason FROM paper_grid_bots WHERE id = $1
	`, seeded).Scan(&closedReason); err != nil {
		t.Fatalf("load closed bot: %v", err)
	}
	if closedReason != "RANGE_BREAK_UP" {
		t.Fatalf("up-break with exhausted budget must close RANGE_BREAK_UP, got %q", closedReason)
	}
	if err := h.pool.QueryRow(context.Background(), `
		SELECT id, lower_price, upper_price, quote_investment
		FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&newID, &newLower, &newUpper, &newInvestment); err != nil {
		t.Fatalf("the DGT re-deploy must leave a fresh RUNNING bot: %v", err)
	}
	center := newLower.Add(newUpper).Div(decimal.NewFromInt(2))
	if center.Sub(decimal.NewFromInt(130)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Fatalf("re-deployed grid must center on the break price 130, got %s", center.StringFixed(6))
	}
	if !newInvestment.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("re-deploy must commit the same slot capital 200, got %s", newInvestment.String())
	}
	var direction, dgtFlag string
	var parentBot int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT direction, COALESCE(model_state->>'dgtRedeploy','false'), 
		       COALESCE(model_state->>'dgtParentBot','0')::INT
		FROM paper_grid_bots WHERE id = $1
	`, newID).Scan(&direction, &dgtFlag, &parentBot); err != nil {
		t.Fatalf("load redeployed bot state: %v", err)
	}
	if direction != "SHORT" || dgtFlag != "true" {
		t.Fatalf("re-deploy must keep the direction and carry the dgt marker, got %s/%s", direction, dgtFlag)
	}

	var events int
	var source string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT COUNT(*), COALESCE(MAX(bot_source), '') FROM bot_execution_events
		WHERE symbol = $1 AND event_type = 'DGT_REDEPLOY'
	`, symbol).Scan(&events, &source); err != nil {
		t.Fatalf("count DGT events: %v", err)
	}
	if events != 1 || source != "PAPER" {
		t.Fatalf("exactly one PAPER DGT_REDEPLOY event expected, got %d (%s)", events, source)
	}
}

// TestPaperDgtRedeploysOnDownBreak pins the down-break nuance: DGT follows
// the market DOWN as well — a LONG bot closed by RANGE_BREAK_DOWN (adverse
// regime unknown) re-opens centered on the lower break price with the same
// capital.
func TestPaperDgtRedeploysOnDownBreak(t *testing.T) {
	const symbol = "DGTD_USDT_PERP"
	h := newGridLifecyclePaperHarness(t, map[string]string{symbol: "70"})
	h.patchSettings(t, "dgt_redeploy_enabled = TRUE, tranche_deploy_enabled = FALSE")
	seeded := h.seedPaperBot(t, symbol, "LONG", 3, "0 minutes")

	if err := h.worker.managePaperBots(context.Background(), h.reloadSettings(t)); err != nil {
		t.Fatalf("managePaperBots: %v", err)
	}

	var closedReason string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT closed_reason FROM paper_grid_bots WHERE id = $1
	`, seeded).Scan(&closedReason); err != nil {
		t.Fatalf("load closed bot: %v", err)
	}
	if closedReason != "RANGE_BREAK_DOWN" {
		t.Fatalf("down-break on adverse regime must close RANGE_BREAK_DOWN, got %q", closedReason)
	}
	var newLower, newUpper, newInvestment decimal.Decimal
	if err := h.pool.QueryRow(context.Background(), `
		SELECT lower_price, upper_price, quote_investment
		FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&newLower, &newUpper, &newInvestment); err != nil {
		t.Fatalf("the DGT re-deploy must leave a fresh RUNNING bot: %v", err)
	}
	center := newLower.Add(newUpper).Div(decimal.NewFromInt(2))
	if center.Sub(decimal.NewFromInt(70)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Fatalf("re-deployed grid must center on the break price 70, got %s", center.StringFixed(6))
	}
	if !newInvestment.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("re-deploy must commit the same slot capital 200, got %s", newInvestment.String())
	}
}

// TestPaperDgtDisabledKeepsOldBehavior: with dgt_redeploy_enabled off, the
// break closes the bot and the slot simply waits for the scanner — no fresh
// bot, no DGT_REDEPLOY event (the pre-v2.0.89-part-B behavior, byte for
// byte).
func TestPaperDgtDisabledKeepsOldBehavior(t *testing.T) {
	const symbol = "DGTX_USDT_PERP"
	h := newGridLifecyclePaperHarness(t, map[string]string{symbol: "130"})
	h.patchSettings(t, "dgt_redeploy_enabled = FALSE, tranche_deploy_enabled = FALSE")
	seeded := h.seedPaperBot(t, symbol, "SHORT", 3, "0 minutes")

	if err := h.worker.managePaperBots(context.Background(), h.reloadSettings(t)); err != nil {
		t.Fatalf("managePaperBots: %v", err)
	}

	var closedReason string
	var running, events int
	_ = h.pool.QueryRow(context.Background(), `
		SELECT closed_reason FROM paper_grid_bots WHERE id = $1
	`, seeded).Scan(&closedReason)
	if err := h.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if err := h.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE symbol = $1 AND event_type IN ('DGT_REDEPLOY', 'DGT_REDEPLOY_SKIPPED')
	`, symbol).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if closedReason != "RANGE_BREAK_UP" || running != 0 || events != 0 {
		t.Fatalf("DGT off must keep the old behavior: close happened (%s) but no redeploy (running=%d events=%d)",
			closedReason, running, events)
	}
}

// TestPaperHalfLifeRotationAgesOutOldBots pins the paper arm of FIX-2 on the
// synthetic HL≈4h tape: max age = clamp(2×4h) = 8h, so a 9h bot closes with
// GRID_AGED_HALF_LIFE (model_state carries halfLifeHours/maxAgeHours) while
// a 5h bot on the same tape stays RUNNING.
func TestPaperHalfLifeRotationAgesOutOldBots(t *testing.T) {
	const oldSymbol = "OUHL_OLD_USDT_PERP"
	const youngSymbol = "OUHL_YNG_USDT_PERP"
	h := newGridLifecyclePaperHarness(t, map[string]string{
		oldSymbol: "100", youngSymbol: "100",
	})
	h.mock.setOUTape(oldSymbol)
	h.mock.setOUTape(youngSymbol)
	h.patchSettings(t, "dgt_redeploy_enabled = TRUE, tranche_deploy_enabled = FALSE")
	oldBot := h.seedPaperBot(t, oldSymbol, "NEUTRAL", 0, "9 hours")
	youngBot := h.seedPaperBot(t, youngSymbol, "NEUTRAL", 0, "5 hours")

	if err := h.worker.managePaperBots(context.Background(), h.reloadSettings(t)); err != nil {
		t.Fatalf("managePaperBots: %v", err)
	}

	var closedReason, halfLife, maxAge string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT closed_reason,
		       COALESCE(model_state->>'halfLifeHours', ''),
		       COALESCE(model_state->>'maxAgeHours', '')
		FROM paper_grid_bots WHERE id = $1
	`, oldBot).Scan(&closedReason, &halfLife, &maxAge); err != nil {
		t.Fatalf("load aged bot: %v", err)
	}
	if closedReason != "GRID_AGED_HALF_LIFE" {
		t.Fatalf("9h bot on an 8h max age must close GRID_AGED_HALF_LIFE, got %q", closedReason)
	}
	hl, _ := strconv.ParseFloat(halfLife, 64)
	ma, _ := strconv.ParseFloat(maxAge, 64)
	if hl < 3.8 || hl > 4.2 {
		t.Fatalf("synthetic tape must report HL≈4h, got %s", halfLife)
	}
	if ma < 7.8 || ma > 8.2 {
		t.Fatalf("max age must be 2×HL≈8h, got %s", maxAge)
	}

	var youngStatus string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT status FROM paper_grid_bots WHERE id = $1
	`, youngBot).Scan(&youngStatus); err != nil {
		t.Fatalf("load young bot: %v", err)
	}
	if youngStatus != "RUNNING" {
		t.Fatalf("5h bot on an 8h max age must stay RUNNING, got %s", youngStatus)
	}

	var events int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE symbol = $1 AND event_type = 'GRID_AGED_HALF_LIFE'
	`, oldSymbol).Scan(&events); err != nil {
		t.Fatalf("count aged events: %v", err)
	}
	if events != 1 {
		t.Fatalf("exactly one GRID_AGED_HALF_LIFE event expected, got %d", events)
	}
	// Aged rotation must NOT leave a DGT re-deploy behind: the freed slot
	// belongs to the scanner.
	var redeploys int
	_ = h.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE symbol = $1 AND event_type = 'DGT_REDEPLOY'
	`, oldSymbol).Scan(&redeploys)
	if redeploys != 0 {
		t.Fatalf("half-life rotation must not DGT-redeploy, got %d events", redeploys)
	}
}

// gridLifecycleRealHarness adds the REAL-fleet pieces to the paper harness:
// one enabled+verified mock account whose private client is the same mock
// server, with the real-execution switches saved/restored around the test.
type gridLifecycleRealHarness struct {
	gridLifecyclePaperHarness
	account accounts.Account
}

func newGridLifecycleRealHarness(t *testing.T, symbol, price string) *gridLifecycleRealHarness {
	t.Helper()
	h := &gridLifecycleRealHarness{
		gridLifecyclePaperHarness: *newGridLifecyclePaperHarness(t, map[string]string{symbol: price}),
	}
	ctx := context.Background()

	accountName := "integration-grid-lifecycle-real-" + time.Now().Format("150405.000000000")
	_, _ = h.pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-grid-lifecycle-real%')`)
	_, _ = h.pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-grid-lifecycle-real%'`)
	account, err := accounts.NewService(h.pool).Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	// realExecutionAllowed demands an enabled account with read permission —
	// Create() ships the UNVERIFIED defaults.
	if _, err := h.pool.Exec(ctx, `
		UPDATE pionex_accounts SET is_enabled = TRUE, has_read_permission = TRUE WHERE id = $1
	`, account.ID); err != nil {
		t.Fatalf("verify account: %v", err)
	}
	var savedAccount *string
	_ = h.pool.QueryRow(ctx, `SELECT account_id FROM autogrid_settings WHERE id = $1`, h.settings.ID).Scan(&savedAccount)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := h.pool.Exec(cleanupCtx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup grid bots: %v", err)
		}
		if _, err := h.pool.Exec(cleanupCtx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
		_, _ = h.pool.Exec(cleanupCtx, `UPDATE autogrid_settings SET account_id = $2 WHERE id = $1`,
			h.settings.ID, savedAccount)
	})

	mockClient := pionex.NewClient(h.mock.server.URL, "itest-key", "itest-secret")
	h.service.clientMu.Lock()
	h.service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	h.service.clientMu.Unlock()
	h.account = *account
	return h
}

// enableRealExecution flips the app_config + feature gates on and restores
// them after the test.
func (h *gridLifecycleRealHarness) enableRealExecution(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `
		UPDATE app_config SET value = 'true'::jsonb WHERE key = 'real_grid_execution_enabled'
	`); err != nil {
		t.Fatalf("enable real_grid_execution_enabled: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE feature_flags SET enabled = TRUE WHERE name = 'real_native_grid'
	`); err != nil {
		t.Fatalf("enable real_native_grid: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE app_config SET value = 'false'::jsonb WHERE key = 'real_grid_execution_enabled'
		`)
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE feature_flags SET enabled = FALSE WHERE name = 'real_native_grid'
		`)
	})
}

// seedRealBot inserts one RUNNING native grid row for the harness account.
func (h *gridLifecycleRealHarness) seedRealBot(
	t *testing.T, symbol, direction string, adjustments int, createdAt string,
) string {
	t.Helper()
	var botID string
	buOrderID := fmt.Sprintf("GLC-%d-%d", time.Now().UnixNano(), adjustments)
	err := h.pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			adjustments_count, created_at, model_state
		) VALUES (
			$1, $2, $3, 'RUNNING', $4,
			'ARITHMETIC', 90, 110, 20, 2,
			200, 0, $5, 'REAL', 'REST_AUTHORITATIVE_OK', $6,
			$7, NOW() - $8::INTERVAL, jsonb_build_object('trancheBase', '200')
		)
		RETURNING id
	`, h.account.ID, h.settings.ID, symbol, direction,
		"itest-"+time.Now().Format("150405.000000000"), buOrderID, adjustments, createdAt).Scan(&botID)
	if err != nil {
		t.Fatalf("seed real bot: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// Events are cleaned BY SYMBOL: the DGT re-deploy writes its event
		// on the NEW bot's id, which the bot_id-scoped delete never covers —
		// a leftover from a previous run breaks the event-count assertions.
		if _, err := h.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM grid_bots WHERE id = $1`, botID); err != nil {
			t.Errorf("cleanup bot: %v", err)
		}
	})
	return botID
}

// TestRealDgtRedeploysAfterTerminalSettle pins the REAL arm of FIX-1 end to
// end through reconcileAndManage: pass one closes the broken bot natively
// (RANGE_BREAK_UP, cancel accepted) and QUEUES the durable intent — no
// duplicate create while the parent still holds the seat; once the exchange
// reports the parent terminal (pass two), the intent executes and a fresh
// native bot appears: same symbol/direction/capital, center at the break
// price, DGT_REDEPLOY event with bot_source REAL.
func TestRealDgtRedeploysAfterTerminalSettle(t *testing.T) {
	const symbol = "DGTR_USDT_PERP"
	h := newGridLifecycleRealHarness(t, symbol, "130")
	h.enableRealExecution(t)
	h.patchSettings(t, "dgt_redeploy_enabled = TRUE, tranche_deploy_enabled = FALSE")
	parent := h.seedRealBot(t, symbol, "SHORT", 3, "2 hours")
	ctx := context.Background()

	// Pass one: RUNNING remotely, price 130 breaks the [90,110] range up,
	// regime fetch (60M klines) answers empty → unknown → RANGE_BREAK_UP.
	if _, err := h.worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass one: %v", err)
	}
	var closedReason, pendingAt string
	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(closed_reason, ''), COALESCE(model_state->>'dgtRedeployPendingAt', '')
		FROM grid_bots WHERE id = $1
	`, parent).Scan(&closedReason, &pendingAt); err != nil {
		t.Fatalf("load parent after pass one: %v", err)
	}
	if closedReason != "RANGE_BREAK_UP" {
		t.Fatalf("pass one must close RANGE_BREAK_UP, got %q", closedReason)
	}
	if pendingAt == "" {
		t.Fatal("pass one must queue the DGT re-deploy intent (dgtRedeployPendingAt)")
	}
	if got := h.mock.cancelCount.Load(); got != 1 {
		t.Fatalf("pass one must submit exactly one native cancel, got %d", got)
	}
	if got := h.mock.createCount.Load(); got != 0 {
		t.Fatalf("no create may ship before the parent settles terminal, got %d", got)
	}

	// Pass two: the exchange reports the parent finished — the settle path
	// writes the terminal state, then the intent executor re-deploys.
	h.mock.orderFinished.Store(true)
	if _, err := h.worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass two: %v", err)
	}

	if got := h.mock.createCount.Load(); got != 1 {
		t.Fatalf("the settled intent must create exactly one native grid, got %d", got)
	}
	body := h.mock.create()
	if body["buOrderData"] == nil {
		t.Fatalf("create body must carry buOrderData, got %v", body)
	}

	var status, direction, newID string
	var lower, upper, investment decimal.Decimal
	if err := h.pool.QueryRow(ctx, `
		SELECT id, status, direction, lower_price, upper_price, quote_investment
		FROM grid_bots
		WHERE account_id = $1 AND symbol = $2 AND bu_order_id IS NOT NULL AND id <> $3
	`, h.account.ID, symbol, parent).Scan(&newID, &status, &direction, &lower, &upper, &investment); err != nil {
		t.Fatalf("the DGT re-deploy must leave a fresh native bot: %v", err)
	}
	if status != "RUNNING" {
		t.Fatalf("re-deployed native bot must be RUNNING, got %s", status)
	}
	if direction != "SHORT" {
		t.Fatalf("re-deploy must keep the SHORT direction, got %s", direction)
	}
	center := lower.Add(upper).Div(decimal.NewFromInt(2))
	if center.Sub(decimal.NewFromInt(130)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Fatalf("re-deployed grid must center on the break price 130, got %s (bounds %s..%s)",
			center.StringFixed(6), lower.StringFixed(6), upper.StringFixed(6))
	}
	if !investment.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("re-deploy must commit the parent's slot capital 200, got %s", investment.String())
	}

	var parentOutcome string
	_ = h.pool.QueryRow(ctx, `
		SELECT COALESCE(model_state->>'dgtRedeployOutcome', '') FROM grid_bots WHERE id = $1
	`, parent).Scan(&parentOutcome)
	if parentOutcome != "deployed" {
		t.Fatalf("parent intent must be consumed as deployed, got %q", parentOutcome)
	}

	var events int
	var source string
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(bot_source), '') FROM bot_execution_events
		WHERE symbol = $1 AND event_type = 'DGT_REDEPLOY'
	`, symbol).Scan(&events, &source); err != nil {
		t.Fatalf("count DGT events: %v", err)
	}
	if events != 1 || source != "REAL" {
		t.Fatalf("exactly one REAL DGT_REDEPLOY event expected, got %d (%s)", events, source)
	}
}

// TestRealHalfLifeRotationClosesAgedBot pins the REAL arm of FIX-2: on the
// HL≈4h tape a 9h RUNNING native bot is rotated out — STOP_REQUESTED with
// closed_reason GRID_AGED_HALF_LIFE, one native cancel, model_state
// telemetry, and the durable event.
func TestRealHalfLifeRotationClosesAgedBot(t *testing.T) {
	const symbol = "OUHLREAL_USDT_PERP"
	h := newGridLifecycleRealHarness(t, symbol, "100")
	h.mock.setOUTape(symbol)
	h.patchSettings(t, "dgt_redeploy_enabled = TRUE, tranche_deploy_enabled = FALSE")
	bot := h.seedRealBot(t, symbol, "NEUTRAL", 0, "9 hours")
	ctx := context.Background()

	if _, err := h.worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcileAndManage: %v", err)
	}

	var status, closedReason, halfLife, maxAge string
	if err := h.pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason, ''),
		       COALESCE(model_state->>'halfLifeHours', ''),
		       COALESCE(model_state->>'maxAgeHours', '')
		FROM grid_bots WHERE id = $1
	`, bot).Scan(&status, &closedReason, &halfLife, &maxAge); err != nil {
		t.Fatalf("load aged REAL bot: %v", err)
	}
	if closedReason != "GRID_AGED_HALF_LIFE" || status != "STOP_REQUESTED" {
		t.Fatalf("9h REAL bot must rotate to STOP_REQUESTED/GRID_AGED_HALF_LIFE, got %s/%s", status, closedReason)
	}
	hl, _ := strconv.ParseFloat(halfLife, 64)
	if hl < 3.8 || hl > 4.2 {
		t.Fatalf("REAL rotation telemetry must report HL≈4h, got %s", halfLife)
	}
	ma, _ := strconv.ParseFloat(maxAge, 64)
	if ma < 7.8 || ma > 8.2 {
		t.Fatalf("REAL rotation telemetry must report maxAge≈8h, got %s", maxAge)
	}
	if got := h.mock.cancelCount.Load(); got != 1 {
		t.Fatalf("half-life rotation must submit exactly one native cancel, got %d", got)
	}
	var events int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'GRID_AGED_HALF_LIFE'
	`, bot).Scan(&events); err != nil {
		t.Fatalf("count aged events: %v", err)
	}
	if events != 1 {
		t.Fatalf("exactly one GRID_AGED_HALF_LIFE event expected, got %d", events)
	}
}
