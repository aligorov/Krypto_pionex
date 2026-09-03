package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// adjustExchangeMock emulates the two native endpoints Service.AdjustBot
// touches for a REAL range shift: the public ticker feed (openPrice) and
// the signed adjustParams call, whose JSON body is captured so tests can
// pin the exact geometry the radar shipped to the exchange.
type adjustExchangeMock struct {
	server      *httptest.Server
	failAdjust  atomic.Bool
	adjustCount atomic.Int64
	mu          sync.Mutex
	lastAdjust  map[string]any
}

func newAdjustExchangeMock(t *testing.T) *adjustExchangeMock {
	t.Helper()
	mock := &adjustExchangeMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "RADR_USDT_PERP", "close": "103.5"},
				},
			},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParams", func(w http.ResponseWriter, r *http.Request) {
		mock.adjustCount.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.mu.Lock()
		mock.lastAdjust = body
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if mock.failAdjust.Load() {
			json.NewEncoder(w).Encode(map[string]any{
				"result": false, "timestamp": time.Now().UnixMilli(),
				"code": "ADJUST_FAIL", "message": "grid cannot be adjusted",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *adjustExchangeMock) adjust() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]any, len(m.lastAdjust))
	for k, v := range m.lastAdjust {
		cp[k] = v
	}
	return cp
}

// radarRealHarness wires a Worker + Service against a disposable DB and the
// adjust mock exchange, owning one verified account per test function.
type radarRealHarness struct {
	pool     *pgxpool.Pool
	worker   *Worker
	service  *Service
	mock     *adjustExchangeMock
	settings *Settings
	account  accounts.Account
}

func newRadarRealHarness(t *testing.T) *radarRealHarness {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newAdjustExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-radar-real-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(context.Background(), `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-radar-real-test%')`)
	_, _ = pool.Exec(context.Background(), `DELETE FROM pionex_accounts WHERE name LIKE 'integration-radar-real-test%'`)
	account, err := accountService.Create(context.Background(), accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup grid bots: %v", err)
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

	settings, err := service.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	return &radarRealHarness{
		pool: pool, worker: worker, service: service, mock: mock,
		settings: settings, account: *account,
	}
}

// seedRealBot inserts one RUNNING native grid bot (2h old — past
// radarMinBotAge) with a stored anti-hunt stop 2 below the lower bound.
func (h *radarRealHarness) seedRealBot(t *testing.T, symbol string, adjustments int) string {
	t.Helper()
	var botID string
	buOrderID := fmt.Sprintf("RADR-%d-%d", time.Now().UnixNano(), adjustments)
	err := h.pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			anti_hunt_stop_price, adjustments_count, created_at
		) VALUES (
			$1, $2, $3, 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			200, 0, $4, 'REAL', 'REST_AUTHORITATIVE_OK', $5,
			88, $6, NOW() - INTERVAL '2 hours'
		)
		RETURNING id
	`, h.account.ID, h.settings.ID, symbol,
		"itest-"+time.Now().Format("150405.000000000"), buOrderID, adjustments).Scan(&botID)
	if err != nil {
		t.Fatalf("seed real bot: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := h.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = $1`, botID); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM bot_risk_snapshots WHERE bot_id = $1`, botID); err != nil {
			t.Errorf("cleanup snapshots: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `DELETE FROM grid_bots WHERE id = $1`, botID); err != nil {
			t.Errorf("cleanup bot: %v", err)
		}
	})
	return botID
}

// seedDwell plants the 3 consecutive over-B3 snapshots the dwell gate
// requires (the current in-flight snapshot counts as one of them in
// production; here the action is invoked directly, so seed the full trio).
func (h *radarRealHarness) seedDwell(t *testing.T, botID, symbol string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if _, err := h.pool.Exec(context.Background(), `
			INSERT INTO bot_risk_snapshots
				(bot_id, bot_number, bot_source, symbol, mode, score, band, captured_at)
			VALUES ($1, 500, 'REAL', $2, 'ACTIVE', 0.80, 3, NOW() - make_interval(mins => $3))
		`, botID, symbol, i+1); err != nil {
			t.Fatalf("seed dwell snapshot: %v", err)
		}
	}
}

func (h *radarRealHarness) seedRadarEvent(t *testing.T, botID, symbol string, ageMinutes int) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO bot_execution_events (
			bot_id, bot_number, bot_source, symbol,
			event_type, price, pnl_usdt, details, created_at
		) VALUES (
			$1, 500, 'REAL', $2, 'ADJUST_RANGE', 100, 0,
			'{"reason":"RADAR_B3_RECENTER","action":"RADAR_RECENTER"}'::jsonb,
			NOW() - make_interval(mins => $3)
		)
	`, botID, symbol, ageMinutes); err != nil {
		t.Fatalf("seed radar event: %v", err)
	}
}

func realRadarAction(symbol string, price decimal.Decimal) radarInput {
	return radarInput{
		botID: "", botNumber: 500, botSource: "REAL", symbol: symbol, direction: "NEUTRAL",
		price: price, lower: decimal.NewFromInt(90), upper: decimal.NewFromInt(110),
		atrEntryPct: 1.0, total: decimal.NewFromInt(-3), inventorySide: 1,
	}
}

// (a) The REAL fleet must land in radarInputs with the durable-column
// semantics — and a SHADOW pass must persist its snapshot with
// bot_source='REAL' so the calibration ledger stays separable.
func TestRealRadarInputsAndSnapshot(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 0)
	// A second RUNNING REAL bot whose symbol has no live price this pass —
	// it must be skipped, never scored against a zero price.
	noPriceID := h.seedRealBot(t, "NORAD_RADR_X_PERP", 0)

	prices := map[string]decimal.Decimal{symbol: decimal.NewFromFloat(103.5)}
	inputs := h.worker.realRadarInputs(ctx, *h.settings, prices)
	var in *radarInput
	for i := range inputs {
		if inputs[i].botID == noPriceID {
			t.Fatal("REAL bot without a live price must be skipped")
		}
		if inputs[i].botSource != "REAL" {
			t.Fatalf("every REAL input must carry botSource=REAL, got %q", inputs[i].botSource)
		}
		if inputs[i].botID == botID {
			in = &inputs[i]
		}
	}
	if in == nil {
		t.Fatalf("radar inputs must include the REAL bot, got %d inputs", len(inputs))
	}
	if in.botNumber == 0 || in.symbol != symbol || in.direction != "NEUTRAL" {
		t.Fatalf("REAL input identity mismatch: %+v", in)
	}
	if !in.price.Equal(decimal.NewFromFloat(103.5)) ||
		!in.lower.Equal(decimal.NewFromInt(90)) || !in.upper.Equal(decimal.NewFromInt(110)) {
		t.Fatalf("REAL input geometry mismatch: price %s lower %s upper %s", in.price, in.lower, in.upper)
	}
	// stored stop 88 must survive as an anchored stop; price above mid on a
	// NEUTRAL grid = short inventory (the grid sold the way up)
	if in.antiHunt == nil || !in.antiHunt.Equal(decimal.NewFromInt(88)) {
		t.Fatalf("stored anti-hunt 88 must anchor the REAL input, got %v", in.antiHunt)
	}
	if in.inventorySide != -1 {
		t.Fatalf("price above mid must be short inventory, got %.0f", in.inventorySide)
	}
	// A bot with no stop at all (column default 0) must read as un-anchored.
	_, _ = h.pool.Exec(ctx, `UPDATE grid_bots SET anti_hunt_stop_price = 0 WHERE id = $1`, botID)
	inputs = h.worker.realRadarInputs(ctx, *h.settings, prices)
	var zeroStop *radarInput
	for i := range inputs {
		if inputs[i].botID == botID {
			zeroStop = &inputs[i]
		}
	}
	if zeroStop == nil || zeroStop.antiHunt != nil {
		t.Fatalf("zero stop column must score as no stop anchored, got %v", zeroStop)
	}
	_, _ = h.pool.Exec(ctx, `UPDATE grid_bots SET anti_hunt_stop_price = 88 WHERE id = $1`, botID)

	// SHADOW pass persists the REAL snapshot.
	h.settings.StopForecastMode = "SHADOW"
	h.worker.radarPass(ctx, *h.settings, inputs)
	var source string
	var mode string
	if err := h.pool.QueryRow(ctx, `
		SELECT bot_source, mode FROM bot_risk_snapshots
		WHERE bot_id = $1 ORDER BY captured_at DESC LIMIT 1
	`, botID).Scan(&source, &mode); err != nil {
		t.Fatalf("REAL snapshot must be persisted: %v", err)
	}
	if source != "REAL" || mode != "SHADOW" {
		t.Fatalf("REAL snapshot must carry bot_source=REAL mode=SHADOW, got %s/%s", source, mode)
	}
}

// (b) B3 dwell on a REAL bot: native adjust_params with the re-center
// geometry (same width centered on price, row preserved), adjustments_count
// +1, anti-hunt re-anchored, and the ADJUST_RANGE event with
// bot_source='REAL' + reason RADAR_B3_RECENTER (which arms the durable
// cooldown).
func TestRadarRealRecenterB3(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 0)
	h.seedDwell(t, botID, symbol)

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{
		Band: 3, Score: 0.80, S1: 0.9, DistToStopATR: 2.0,
	})

	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("expected exactly one native adjust_params, got %d", got)
	}
	body := h.mock.adjust()
	if body["type"] != "adjust_params" {
		t.Fatalf("adjust must be adjust_params, got %v", body["type"])
	}
	if body["bottom"] != "93.5" || body["top"] != "113.5" {
		t.Fatalf("re-center must ship [93.5, 113.5] (width 20 at price 103.5), got %v..%v", body["bottom"], body["top"])
	}
	if body["row"] != float64(20) {
		t.Fatalf("row must stay the existing grid_num 20, got %v", body["row"])
	}

	var lower, upper, antiHunt decimal.Decimal
	var adjustments int
	if err := h.pool.QueryRow(ctx, `
		SELECT lower_price, upper_price, anti_hunt_stop_price, adjustments_count
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&lower, &upper, &antiHunt, &adjustments); err != nil {
		t.Fatalf("load bot after recenter: %v", err)
	}
	if !lower.Equal(d("93.5")) || !upper.Equal(d("113.5")) {
		t.Fatalf("persisted bounds must be [93.5, 113.5], got [%s, %s]", lower, upper)
	}
	if adjustments != 1 {
		t.Fatalf("adjustments_count must be incremented exactly once, got %d", adjustments)
	}
	// Stop keeps its 2-below-lower distance: 88 → 91.5.
	if !antiHunt.Equal(d("91.5")) {
		t.Fatalf("anti-hunt must re-anchor to 91.5, got %s", antiHunt)
	}

	var reason, source string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason', bot_source FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&reason, &source); err != nil {
		t.Fatalf("recenter event must be logged: %v", err)
	}
	if reason != "RADAR_B3_RECENTER" || source != "REAL" {
		t.Fatalf("event must be REAL/RADAR_B3_RECENTER, got %s/%s", source, reason)
	}
}

// (c) The durable cooldown gates REAL ids: a radar re-center 30 minutes ago
// blocks at 2σ (2h window) — restart-proof, same ledger as paper.
func TestRadarRealDurableCooldown(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 0)
	h.seedDwell(t, botID, symbol)
	h.seedRadarEvent(t, botID, symbol, 30)

	lastAt, ok := h.worker.radarLastActionAt(ctx, botID)
	if !ok || time.Since(lastAt) < 29*time.Minute {
		t.Fatalf("REAL radar action must be durable-visible, ok=%v age=%v", ok, time.Since(lastAt))
	}

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{
		Band: 3, Score: 0.80, DistToStopATR: 2.0, // → 2h window, event 30m old
	})
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("cooldown must block the REAL recenter, got %d adjusts", got)
	}
	var adjustments int
	if err := h.pool.QueryRow(ctx, `SELECT adjustments_count FROM grid_bots WHERE id = $1`, botID).Scan(&adjustments); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if adjustments != 0 {
		t.Fatalf("blocked recenter must not touch the budget, got %d", adjustments)
	}
}

// (d) Budget 5/5 blocks B3; the B4 escape lane is exactly one shift beyond
// the budget and never more.
func TestRadarRealBudgetAndB4Escape(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	atMax := h.seedRealBot(t, symbol, 5) // budget fully spent
	h.seedDwell(t, atMax, symbol)

	h.settings.StopForecastMode = "ACTIVE"
	h.settings.MaxAdjustmentsPerBot = 5
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = atMax

	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("B3 at budget 5/5 must be blocked, got %d adjusts", got)
	}

	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 4, Score: 0.92, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("B4 must spend the single escape slot, got %d adjusts", got)
	}
	var adjustments int
	if err := h.pool.QueryRow(ctx, `SELECT adjustments_count FROM grid_bots WHERE id = $1`, atMax).Scan(&adjustments); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if adjustments != 6 {
		t.Fatalf("escape must leave count at max+1, got %d", adjustments)
	}

	// A fresh bot already at max+1: the escape slot is spent, B4 must not
	// open a second one.
	spent := h.seedRealBot(t, symbol, 6)
	h.seedDwell(t, spent, symbol)
	b.botID = spent
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 4, Score: 0.95, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("second B4 escape must be blocked, got %d adjusts", got)
	}
}

// (e) A failing native adjust leaves everything untouched: no budget spend,
// no bounds change, no cooldown-arming event — the failure is Warned, not
// swallowed into a phantom shift.
func TestRadarRealAdjustFailureLeavesBudgetUntouched(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 2)
	h.seedDwell(t, botID, symbol)
	h.mock.failAdjust.Store(true)

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{
		Band: 3, Score: 0.80, DistToStopATR: 2.0,
	})

	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("the exchange must have been asked once, got %d", got)
	}
	var lower, upper, antiHunt decimal.Decimal
	var adjustments, events int
	if err := h.pool.QueryRow(ctx, `
		SELECT lower_price, upper_price, anti_hunt_stop_price, adjustments_count
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&lower, &upper, &antiHunt, &adjustments); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if !lower.Equal(d("90")) || !upper.Equal(d("110")) || !antiHunt.Equal(d("88")) || adjustments != 2 {
		t.Fatalf("failed adjust must change nothing, got [%s,%s] stop %s count %d", lower, upper, antiHunt, adjustments)
	}
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE'
	`, botID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("failed adjust must not log an ADJUST_RANGE event (cooldown stays unarmed), got %d", events)
	}
}
