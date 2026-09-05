package autogrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// migration0045Path locates the v2.0.89 migration file from the package
// directory (backend/internal/autogrid → repo root /migrations). The test
// skips when the repository layout is not available (module-only checkout).
func migration0045Path(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{
		"../../migrations/0045_ledger_fees_terminal_truth.sql",
		"/app/migrations/0045_ledger_fees_terminal_truth.sql",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("migration 0045 file not reachable from this checkout; skipping SQL==Go pin")
	return ""
}

// ledgerFeeEnv is a minimal disposable REAL fleet for the fee-ledger tests.
type ledgerFeeEnv struct {
	pool     *pgxpool.Pool
	service  *Service
	account  *accounts.Account
	settings *Settings
	teardown []func()
}

func newLedgerFeeEnv(t *testing.T) *ledgerFeeEnv {
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

	accountName := "integration-ledgerfees-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-ledgerfees%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-ledgerfees%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-ledgerfees%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	env := &ledgerFeeEnv{pool: pool, service: service, account: account}
	t.Cleanup(func() {
		for i := len(env.teardown) - 1; i >= 0; i-- {
			env.teardown[i]()
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $1)`, account.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})

	var savedAccountID *string
	var savedStatus, savedMode string
	if err := pool.QueryRow(ctx, `
		SELECT account_id, status, execution_mode
		FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID, &savedStatus, &savedMode); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, status = 'RUNNING', execution_mode = 'REAL'
		WHERE scope_key = $1::VARCHAR
	`, DefaultScope, account.ID); err != nil {
		t.Fatalf("retarget settings: %v", err)
	}
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	env.settings = settings
	return env
}

// seedLedgerBot lands one terminal REAL bot in the exact pre-0045 state the
// epoch carried: a settled marker, an investment, a max_loss and a close
// reason. Returns the bot id.
func (env *ledgerFeeEnv) seedLedgerBot(
	t *testing.T, symbol, marker, closedReason string, investment string, leverage int, maxLoss string,
) string {
	t.Helper()
	var botID string
	modelState := map[string]any{}
	if marker != "" {
		modelState["finalProfitSource"] = marker
	}
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			realized_pnl_usdt, unrealized_pnl_usdt, closed_reason, max_loss_usdt,
			closed_at, created_at, model_state
		) VALUES (
			$1, $2, $3, 'STOPPED', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, $4,
			$5, 0, $6, 'REAL', 'REMOTE_TERMINAL_CONFIRMED', $7,
			2.5, 0, $8, $9,
			NOW() - INTERVAL '2 hours', NOW() - INTERVAL '3 days', $10::JSONB
		)
		RETURNING id
	`, env.account.ID, env.settings.ID, symbol, leverage, investment,
		"itest-"+time.Now().Format("150405.000000000")+"-"+symbol,
		"BU-"+symbol, closedReason, maxLoss, mustJSON(t, modelState)).Scan(&botID); err != nil {
		t.Fatalf("seed ledger bot %s: %v", symbol, err)
	}
	return botID
}

func (env *ledgerFeeEnv) seedLedgerTelemetry(t *testing.T, botID, total, inventory string, age time.Duration) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO bot_telemetry
			(bot_id, bot_number, symbol, captured_at, price, total_pnl, inventory_notional)
		VALUES ($1, 0, 'LEDGER', NOW() - $2::INTERVAL, 100, $3::NUMERIC, $4::NUMERIC)
	`, botID, age.String(), total, inventory); err != nil {
		t.Fatalf("seed ledger telemetry: %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return encoded
}

// TestMigration0045MatchesGoEstimate pins SQL == Go: applying the migration
// file onto a fleet seeded in the exact pre-v2.0.89 state must re-settle
// every non-exchange-total terminal at EXACTLY the figure the worker's
// estimate ladder (terminalTelemetryEstimate) computes, book the close cost
// into fees_paid_usdt, backfill the entry fee, and leave no telemetryless
// final invented. A second application must change nothing (exactly-once).
func TestMigration0045MatchesGoEstimate(t *testing.T) {
	env := newLedgerFeeEnv(t)
	ctx := context.Background()
	path := migration0045Path(t)
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	// The pre-state fleet:
	//   LFRES — grid_funding_residual marker (the epoch's lying leg), manage
	//           stop whose telemetry mark is deeper than the stop;
	//   LFCAS — cascade native stop, positive stale mark (the APT/ICP shape);
	//   LFNOE — no telemetry at all (NULL final, never invented);
	//   LFTOT — an exchange-total final: must NOT be re-settled.
	resID := env.seedLedgerBot(t, "LFRES_USDT_PERP", "grid_funding_residual",
		"STOP_LOSS", "100", 6, "12")
	env.seedLedgerTelemetry(t, resID, "-13.780395", "298.733175", 125*time.Minute)

	casID := env.seedLedgerBot(t, "LFCAS_USDT_PERP", "refused_grid_funding_residual",
		"STOP_LOSS_NATIVE", "100", 6, "12")
	env.seedLedgerTelemetry(t, casID, "-2.268556", "276.392934", 125*time.Minute)

	noTelID := env.seedLedgerBot(t, "LFNOE_USDT_PERP", "none",
		"USER_CANCEL", "50", 2, "0")

	totID := env.seedLedgerBot(t, "LFTOT_USDT_PERP", "total_profit_alias",
		"TAKE_PROFIT", "50", 2, "0")

	apply := func() {
		if _, err := env.pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration 0045: %v", err)
		}
	}
	apply()

	row := func(botID string) (realized *decimal.Decimal, marker string, fees decimal.Decimal, closeCost *decimal.Decimal) {
		t.Helper()
		if err := env.pool.QueryRow(ctx, `
			SELECT realized_pnl_usdt, COALESCE(model_state->>'finalProfitSource',''),
			       fees_paid_usdt, NULLIF(model_state->>'closeCostUsdt','')::NUMERIC
			FROM grid_bots WHERE id = $1
		`, botID).Scan(&realized, &marker, &fees, &closeCost); err != nil {
			t.Fatalf("load bot %s: %v", botID, err)
		}
		return
	}

	// The Go ladder on the same inputs — the pinned contract.
	goFinal, goCost := terminalTelemetryEstimate(
		mustDec(t, "-13.780395"), mustDec(t, "298.733175"), mustDec(t, "12"), "STOP_LOSS")
	realized, marker, fees, closeCost := row(resID)
	if realized == nil || !realized.Equal(goFinal) {
		t.Fatalf("residual row must settle at the Go estimate %s, got %v", goFinal, realized)
	}
	if marker != string(pionex.FinalProfitTelemetryNetClose) {
		t.Fatalf("marker = %q, want telemetry_net_close", marker)
	}
	if closeCost == nil || !closeCost.Equal(goCost) {
		t.Fatalf("closeCostUsdt = %v, want %s", closeCost, goCost)
	}
	// Fees: entry backfill (0.0005×100×6 = 0.3) + close cost.
	if !fees.Equal(mustDec(t, "0.3").Add(goCost)) {
		t.Fatalf("fees = %s, want entry 0.3 + close %s", fees, goCost)
	}

	// The cascade shape floors at the stop.
	goCasFinal, goCasCost := terminalTelemetryEstimate(
		mustDec(t, "-2.268556"), mustDec(t, "276.392934"), mustDec(t, "12"), "STOP_LOSS_NATIVE")
	realized, marker, fees, _ = row(casID)
	if realized == nil || !realized.Equal(goCasFinal) {
		t.Fatalf("cascade row must floor at %s, got %v", goCasFinal, realized)
	}
	if marker != string(pionex.FinalProfitTelemetryNetClose) {
		t.Fatalf("cascade marker = %q, want telemetry_net_close", marker)
	}
	if !fees.Equal(mustDec(t, "0.3").Add(goCasCost)) {
		t.Fatalf("cascade fees = %s, want entry 0.3 + close %s", fees, goCasCost)
	}

	// No telemetry → NULL final, marked none, entry fee only.
	realized, marker, fees, _ = row(noTelID)
	if realized != nil {
		t.Fatalf("no-telemetry row must stay NULL, got %v", realized)
	}
	if marker != string(pionex.FinalProfitNone) {
		t.Fatalf("no-telemetry marker = %q, want none", marker)
	}
	if !fees.Equal(mustDec(t, "0.05")) {
		t.Fatalf("no-telemetry fees = %s, want the entry backfill 0.05", fees)
	}

	// The exchange-total final is untouched (realized kept, no close cost).
	realized, marker, fees, _ = row(totID)
	if realized == nil || !realized.Equal(mustDec(t, "2.5")) {
		t.Fatalf("exchange-total row must keep its final, got %v", realized)
	}
	if marker != "total_profit_alias" {
		t.Fatalf("exchange-total marker = %q, want total_profit_alias", marker)
	}
	if !fees.Equal(mustDec(t, "0.05")) {
		t.Fatalf("exchange-total fees = %s, want the entry backfill 0.05", fees)
	}

	// Exactly-once: re-applying the migration changes nothing.
	before := map[string]string{}
	for _, id := range []string{resID, casID, noTelID, totID} {
		var realizedText, feesText string
		if err := env.pool.QueryRow(ctx, `
			SELECT COALESCE(realized_pnl_usdt::TEXT,'NULL'), fees_paid_usdt::TEXT
			FROM grid_bots WHERE id = $1
		`, id).Scan(&realizedText, &feesText); err != nil {
			t.Fatalf("snapshot row: %v", err)
		}
		before[id] = realizedText + "|" + feesText
	}
	apply()
	for _, id := range []string{resID, casID, noTelID, totID} {
		var realizedText, feesText string
		if err := env.pool.QueryRow(ctx, `
			SELECT COALESCE(realized_pnl_usdt::TEXT,'NULL'), fees_paid_usdt::TEXT
			FROM grid_bots WHERE id = $1
		`, id).Scan(&realizedText, &feesText); err != nil {
			t.Fatalf("recheck row: %v", err)
		}
		if before[id] != realizedText+"|"+feesText {
			t.Fatalf("second application must be a no-op for %s: %q → %q", id, before[id], realizedText+"|"+feesText)
		}
	}
}

// TestEpochSubtractsFeesExactlyOnce pins the epoch formula's fee accounting:
// entry fees subtract once per epoch bot, and the close cost that already
// rides INSIDE a telemetry_net_close final is NOT subtracted again (the
// fees_paid column carries it, model_state.closeCostUsdt nets it out).
func TestEpochSubtractsFeesExactlyOnce(t *testing.T) {
	env := newLedgerFeeEnv(t)
	ctx := context.Background()

	// One running bot: realized 1.0, floating 0.25, entry fee 0.15 booked.
	runningID := "BU-LF-RUN"
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			realized_pnl_usdt, unrealized_pnl_usdt, fees_paid_usdt
		) VALUES (
			$1, $2, 'LFRUN_USDT_PERP', 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $3, 'REAL', 'REST_AUTHORITATIVE_OK', $4,
			1.0, 0.25, 0.15
		)
	`, env.account.ID, env.settings.ID,
		"itest-"+time.Now().Format("150405.000000000"), runningID); err != nil {
		t.Fatalf("seed running bot: %v", err)
	}

	// One closed bot settled at a telemetry_net_close estimate: final −1.2
	// (mark −1.0 − close cost 0.2), fees_paid 0.5 = entry 0.3 + close 0.2,
	// closeCostUsdt 0.2 in model_state.
	closedID := "BU-LF-CLS"
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			realized_pnl_usdt, unrealized_pnl_usdt, fees_paid_usdt,
			closed_reason, closed_at, model_state
		) VALUES (
			$1, $2, 'LFCLS_USDT_PERP', 'STOPPED', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $3, 'REAL', 'REMOTE_TERMINAL_CONFIRMED', $4,
			-1.2, 0, 0.5,
			'STOP_LOSS', NOW() - INTERVAL '1 hour',
			'{"finalProfitSource":"telemetry_net_close","closeCostUsdt":"0.2"}'::JSONB
		)
	`, env.account.ID, env.settings.ID,
		"itest-"+time.Now().Format("150405.000000000"), closedID); err != nil {
		t.Fatalf("seed closed bot: %v", err)
	}

	epochStart := time.Now().UTC().Add(-24 * time.Hour)
	breakdown, err := ComputeEquityBreakdown(ctx, env.pool, env.account.ID, epochStart)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	// Running leg: 1.0 + 0.25 − entry 0.15.
	if !breakdown.RunningPnL.Equal(mustDec(t, "1.1")) {
		t.Fatalf("running PnL = %s, want 1.1 (realized+floating−entry fee)", breakdown.RunningPnL)
	}
	if !breakdown.RunningFeesPaid.Equal(mustDec(t, "0.15")) {
		t.Fatalf("running fees = %s, want 0.15", breakdown.RunningFeesPaid)
	}
	// Closed fee leg: fees_paid 0.5 MINUS the close cost 0.2 already netted
	// inside the stored final — entry-only subtraction.
	if !breakdown.ClosedFeesPaid.Equal(mustDec(t, "0.3")) {
		t.Fatalf("closed fees = %s, want 0.3 (entry only; close cost already inside the final)", breakdown.ClosedFeesPaid)
	}
	if !breakdown.ClosedKnown.Equal(mustDec(t, "-1.2")) {
		t.Fatalf("closed known = %s, want -1.2", breakdown.ClosedKnown)
	}
	// Headline: 1.1 − 1.2 − 0.3 = −0.4 — every dollar subtracted once.
	if !breakdown.EpochPnL().Equal(mustDec(t, "-0.4")) {
		t.Fatalf("epoch PnL = %s, want -0.4", breakdown.EpochPnL())
	}
}

// investInMock serves the two surfaces a REAL invest_in touches: the ticker
// (openPrice gate) and the native adjustParams endpoint.
type investInMock struct {
	server *httptest.Server
	mu     sync.Mutex
	adjust int
}

func newInvestInMock(t *testing.T) *investInMock {
	t.Helper()
	mock := &investInMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": []map[string]any{
				{"symbol": "LFTOP_USDT_PERP", "close": "100"},
			}},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParams", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.adjust++
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// TestInvestInBooksEntryFee drives a REAL invest_in through AdjustBot (the
// single chokepoint every pour — tranche-2 and manual top-ups alike — passes
// through) and pins the fee booking: taker 0.05% × pour × leverage lands in
// fees_paid_usdt in the SAME exactly-once UPDATE as the pour itself.
func TestInvestInBooksEntryFee(t *testing.T) {
	env := newLedgerFeeEnv(t)
	ctx := context.Background()

	mock := newInvestInMock(t)
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	env.service.clientMu.Lock()
	env.service.clientCache[env.account.ID] = &clientCacheEntry{
		fingerprint: env.account.KeyFingerprint, client: mockClient,
	}
	env.service.clientMu.Unlock()

	if _, err := env.pool.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = false, max_daily_loss_usd = 10000,
		    max_account_exposure_usd = 100000, max_symbol_exposure_usd = 100000,
		    max_leverage = 10, max_active_grid_bots = 10
		WHERE id = 1
	`); err != nil {
		t.Fatalf("pin risk settings: %v", err)
	}

	buOrderID := "BU-LF-TOP"
	var botID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id, fees_paid_usdt
		) VALUES (
			$1, $2, 'LFTOP_USDT_PERP', 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 4,
			100, 0, $3, 'REAL', 'REST_AUTHORITATIVE_OK', $4, 0.2
		)
		RETURNING id
	`, env.account.ID, env.settings.ID,
		"itest-"+time.Now().Format("150405.000000000"), buOrderID).Scan(&botID); err != nil {
		t.Fatalf("seed pour bot: %v", err)
	}

	if _, err := env.service.AdjustBot(ctx, accounts.NewService(env.pool),
		env.settings.ID, botID, AdjustBotInput{
			Mode:            "invest_in",
			QuoteInvestment: mustDec(t, "50"),
		}); err != nil {
		t.Fatalf("invest_in: %v", err)
	}

	var investment, fees decimal.Decimal
	if err := env.pool.QueryRow(ctx, `
		SELECT quote_investment, fees_paid_usdt FROM grid_bots WHERE id = $1
	`, botID).Scan(&investment, &fees); err != nil {
		t.Fatalf("load pour bot: %v", err)
	}
	if !investment.Equal(mustDec(t, "150")) {
		t.Fatalf("quote_investment after pour = %s, want 150", investment)
	}
	// 0.2 (deploy fee already on the row) + 0.0005 × 50 × lev 4 = 0.1.
	if !fees.Equal(mustDec(t, "0.3")) {
		t.Fatalf("fees after pour = %s, want 0.3 (0.2 deploy + 0.1 pour)", fees)
	}
	if got := mock.adjustCount(); got != 1 {
		t.Fatalf("exactly one native adjustParams call, got %d", got)
	}
}

func (m *investInMock) adjustCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adjust
}
