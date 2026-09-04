package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// settingsInputFrom mirrors a loaded Settings row into the full
// UpdateSettingsInput form — the same round-trip the MCP/HTTP layers submit.
func settingsInputFrom(s Settings) UpdateSettingsInput {
	return UpdateSettingsInput{
		AccountID:               s.AccountID,
		ExecutionMode:           s.ExecutionMode,
		ScanMode:                s.ScanMode,
		BudgetUSDT:              s.BudgetUSDT,
		MaxActiveBots:           s.MaxActiveBots,
		Leverage:                s.Leverage,
		MinSharpe:               s.MinSharpe,
		MinEVPct:                s.MinEVPct,
		StopLossMode:            s.StopLossMode,
		SmartPNLEnabled:         s.SmartPNLEnabled,
		AdaptiveLeverageEnabled: s.AdaptiveLeverageEnabled,
		DensityGridEnabled:      s.DensityGridEnabled,
		CandleInterval:          s.CandleInterval,
		LookbackCandles:         s.LookbackCandles,
		MaxSymbolsPerScan:       s.MaxSymbolsPerScan,
		ScanIntervalSeconds:     s.ScanIntervalSeconds,
		MinVolume24h:            s.MinVolume24h,
		MinVolatilityPct:        s.MinVolatilityPct,
		MaxVolatilityPct:        s.MaxVolatilityPct,
		MaxDrawdownPct:          s.MaxDrawdownPct,
		MinProfitFactor:         s.MinProfitFactor,
		FeeBps:                  s.FeeBps,
		SlippageBps:             s.SlippageBps,
		PnLTargetMode:           s.PnLTargetMode,
		PnLTargetUSDT:           s.PnLTargetUSDT,
		MaxLossUSDT:             s.MaxLossUSDT,
		ManageIntervalSeconds:   s.ManageIntervalSeconds,
		RangeBreakBufferPct:     s.RangeBreakBufferPct,
		MaxAdjustmentsPerBot:    s.MaxAdjustmentsPerBot,
		StopForecastMode:        s.StopForecastMode,
		AIKitEnabled:            s.AIKitEnabled,
		AIAutotuneEnabled:       s.AIAutotuneEnabled,
		AIAutotuneInterval:      s.AIAutotuneInterval,
	}
}

// riskBreakerFixture snapshots and restores every risk_settings column these
// tests touch, then pins the fleet-design shape under test.
type riskBreakerFixture struct {
	pool  *pgxpool.Pool
	saved struct {
		mode, target, stop, breaker, breakerMode, exposure, symbolExposure string
		tranche, adaptiveLev                                               bool
		maxBots, maxGridBots, leverage                                     int
		budget                                                             string
	}
}

func pinBreakerFixture(t *testing.T, pool *pgxpool.Pool, settingsID string, maxActiveGridBots int, breaker string) *riskBreakerFixture {
	t.Helper()
	ctx := context.Background()
	f := &riskBreakerFixture{pool: pool}
	if err := pool.QueryRow(ctx, `
		SELECT pnl_target_mode, pnl_target_usdt::TEXT, max_loss_usdt::TEXT, tranche_deploy_enabled,
		       max_active_bots, leverage, budget_usdt::TEXT, adaptive_leverage_enabled
		FROM autogrid_settings WHERE id = $1
	`, settingsID).Scan(&f.saved.mode, &f.saved.target, &f.saved.stop, &f.saved.tranche,
		&f.saved.maxBots, &f.saved.leverage, &f.saved.budget, &f.saved.adaptiveLev); err != nil {
		t.Fatalf("snapshot autogrid_settings: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT max_daily_loss_usd::TEXT, breaker_mode, max_account_exposure_usd::TEXT,
		       max_symbol_exposure_usd::TEXT, max_active_grid_bots
		FROM risk_settings WHERE id = 1
	`).Scan(&f.saved.breaker, &f.saved.breakerMode, &f.saved.exposure, &f.saved.symbolExposure,
		&f.saved.maxGridBots); err != nil {
		t.Fatalf("snapshot risk_settings: %v", err)
	}
	t.Cleanup(func() {
		_, err1 := pool.Exec(context.Background(), `
			UPDATE autogrid_settings
			SET pnl_target_mode = $1, pnl_target_usdt = $2::NUMERIC, max_loss_usdt = $3::NUMERIC,
			    tranche_deploy_enabled = $4, max_active_bots = $5, leverage = $6,
			    budget_usdt = $7::NUMERIC, adaptive_leverage_enabled = $8
			WHERE id = $9
		`, f.saved.mode, f.saved.target, f.saved.stop, f.saved.tranche, f.saved.maxBots,
			f.saved.leverage, f.saved.budget, f.saved.adaptiveLev, settingsID)
		_, err2 := pool.Exec(context.Background(), `
			UPDATE risk_settings
			SET max_daily_loss_usd = $1::NUMERIC, breaker_mode = $2,
			    max_account_exposure_usd = $3::NUMERIC, max_symbol_exposure_usd = $4::NUMERIC,
			    max_active_grid_bots = $5
			WHERE id = 1
		`, f.saved.breaker, f.saved.breakerMode, f.saved.exposure, f.saved.symbolExposure, f.saved.maxGridBots)
		if err1 != nil || err2 != nil {
			t.Errorf("restore fixture: %v / %v", err1, err2)
		}
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET budget_usdt = 200, leverage = 4, max_active_bots = 10,
		    tranche_deploy_enabled = TRUE, adaptive_leverage_enabled = FALSE
		WHERE id = $1
	`, settingsID); err != nil {
		t.Fatalf("pin autogrid_settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings
		SET max_daily_loss_usd = $1::NUMERIC, max_account_exposure_usd = 100000,
		    max_symbol_exposure_usd = 100000, max_active_grid_bots = $2,
		    kill_switch_enabled = FALSE
		WHERE id = 1
	`, breaker, maxActiveGridBots); err != nil {
		t.Fatalf("pin risk_settings: %v", err)
	}
	return f
}

// breakerValue reads the live derived breaker for assertions.
func breakerValue(t *testing.T, pool *pgxpool.Pool) (decimal.Decimal, string) {
	t.Helper()
	var value decimal.Decimal
	var mode string
	if err := pool.QueryRow(context.Background(),
		`SELECT max_daily_loss_usd, breaker_mode FROM risk_settings WHERE id = 1`,
	).Scan(&value, &mode); err != nil {
		t.Fatalf("read breaker: %v", err)
	}
	return value, mode
}

// TestUpdateSettingsDerivesBreaker pins the AUTO/MANUAL ownership contract:
// with breaker_mode AUTO the trailing derivation inside Service.UpdateSettings
// re-derives max_daily_loss_usd from the fleet design (N=10/$200/4x/tranches
// → $100); with MANUAL the operator pin survives untouched.
func TestUpdateSettingsDerivesBreaker(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	_, service, live := newCooldownTestWorker(t, pool)
	pinBreakerFixture(t, pool, live.ID, 11, "50")

	input := settingsInputFrom(*mustSettings(t, service, live.ID))
	input.MaxActiveBots = 10
	input.BudgetUSDT = decimal.NewFromInt(200)
	input.Leverage = 4

	// AUTO: the derivation owns the number — 10×200×4×2%/2×1.25 = $100.
	if _, err := pool.Exec(ctx, `UPDATE risk_settings SET breaker_mode = 'AUTO' WHERE id = 1`); err != nil {
		t.Fatalf("set AUTO: %v", err)
	}
	if _, err := service.UpdateSettings(ctx, input); err != nil {
		t.Fatalf("UpdateSettings (AUTO): %v", err)
	}
	breaker, mode := breakerValue(t, pool)
	if !breaker.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("AUTO must derive breaker $100 from N=10/$200/4x/tranche, got %s", breaker)
	}
	if mode != "AUTO" {
		t.Fatalf("derivation must keep breaker_mode AUTO, got %s", mode)
	}

	// MANUAL: the operator pin is untouchable.
	if _, err := pool.Exec(ctx,
		`UPDATE risk_settings SET breaker_mode = 'MANUAL', max_daily_loss_usd = 37.5 WHERE id = 1`); err != nil {
		t.Fatalf("set MANUAL: %v", err)
	}
	if _, err := service.UpdateSettings(ctx, input); err != nil {
		t.Fatalf("UpdateSettings (MANUAL): %v", err)
	}
	breaker, mode = breakerValue(t, pool)
	if !breaker.Equal(decimal.NewFromFloat(37.5)) || mode != "MANUAL" {
		t.Fatalf("MANUAL pin must survive settings updates, got %s / %s", breaker, mode)
	}
}

func mustSettings(t *testing.T, service *Service, id string) *Settings {
	t.Helper()
	settings, err := service.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	return settings
}

// TestTranche2DerivedCapGate pins the v2.0.75 per-bot cap on a live gate:
// budget×leverage×5% CEILING×1.25 — 2x $8 ≤ $25 and 4x $16 ≤ $50 pass, and
// the wide σ-scaled stop class the floor-based cap refused (prod SKYAI 6x
// $21.57 > $15 skipped ×3) now passes; a $40 stop on 6x/$100 exceeds the
// $37.50 ceiling cap and is skipped with the actual cap printed. The breaker
// is pinned high so only the cap speaks.
func TestTranche2DerivedCapGate(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, _, live := newCooldownTestWorker(t, pool)
	pinBreakerFixture(t, pool, live.ID, 11, "1000")
	sixFigure := *mustSettings(t, worker.service, live.ID)
	sixFigure.BudgetUSDT = decimal.NewFromInt(100)

	ghostBot := "00000000-0000-0000-0000-000000000001"
	if skip := worker.tranche2RiskGate(ctx, sixFigure, ghostBot, 2, decimal.NewFromInt(8)); skip != "" {
		t.Fatalf("2x bot $8 must pass the derived $25 cap, got %q", skip)
	}
	if skip := worker.tranche2RiskGate(ctx, sixFigure, ghostBot, 4, decimal.NewFromInt(16)); skip != "" {
		t.Fatalf("4x bot $16 must pass the derived $50 cap, got %q", skip)
	}
	// The prod SKYAI case: a $21.57 dynamic stop on 6x/$100 fits under the
	// $37.50 ceiling the stop formula itself defines.
	if skip := worker.tranche2RiskGate(ctx, sixFigure, ghostBot, 6, decimal.NewFromFloat(21.57)); skip != "" {
		t.Fatalf("6x bot $21.57 must pass the derived $37.50 cap (prod SKYAI case), got %q", skip)
	}
	skip := worker.tranche2RiskGate(ctx, sixFigure, ghostBot, 6, decimal.NewFromInt(40))
	if skip == "" {
		t.Fatalf("$40 must be skipped above the derived $37.50 cap")
	}
	if !strings.Contains(skip, "37.5") || !strings.Contains(skip, "40") {
		t.Fatalf("skip reason must print the actual cap and stop, got %q", skip)
	}
}

// seedFleetTickers stubs the public ticker feed for one deploy symbol.
func seedFleetTickers(t *testing.T, worker *Worker, symbol string) {
	t.Helper()
	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": symbol, "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")
}

// seedFleetScan inserts a fresh SUCCEEDED scan with an ACCEPTED candidate
// for the symbol; returns the scan id.
func seedFleetScan(t *testing.T, pool *pgxpool.Pool, symbol string) string {
	t.Helper()
	var scanID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID)
	})
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, current_price, lower_price, upper_price,
			grid_num, recommended_trend, model_assumptions
		) VALUES (
			$1, $2, 'ACCEPTED', 100, 90, 110,
			10, 'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		)
	`, scanID, symbol); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	return scanID
}

// TestDerivedBreakerN10FleetEnvelope drives the deploy path at the target
// prod shape: N=10/$200/4x/tranches → derived breaker $100, envelope $80.
// Nine 4x fillers store Σ$72 of tranche-1 halves; the tenth deploy (full
// stop reserve $8) lands exactly at the ceiling (72+8 = 80) and MUST pass —
// this is the deadlock the static $50 breaker produced. The eleventh is
// refused by the same envelope (76 + 8 = 84 > 80).
func TestDerivedBreakerN10FleetEnvelope(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, service, live := newCooldownTestWorker(t, pool)
	pinBreakerFixture(t, pool, live.ID, 11, "100")
	// STATIC 6/8 gives every candidate a FULL stop of $8 regardless of
	// leverage (tranche stores the $4 half).
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET pnl_target_mode = 'STATIC', pnl_target_usdt = 6, max_loss_usdt = 8
		WHERE id = $1
	`, live.ID); err != nil {
		t.Fatalf("pin STATIC targets: %v", err)
	}
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("reload pinned settings: %v", err)
	}
	if got := DeriveDailyLossBreaker(*settings); !got.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("fixture must derive the $100 breaker, got %s", got)
	}

	const symbol = "FLEET10_USDT_PERP"
	const symbol11 = "FLEET10B_USDT_PERP"
	seedFleetTickers(t, worker, symbol)
	tradedTF := normalizeBacktestTF(settings.CandleInterval)
	for _, tf := range append([]string{tradedTF}, neighborBacktestTFs(tradedTF)...) {
		for _, sym := range []string{symbol, symbol11} {
			if _, err := pool.Exec(ctx, `
				INSERT INTO backtest_jobs (symbol, interval, status, result, finished_at)
				VALUES ($1, $2, 'DONE',
				        '{"folds": 4, "oos_return_pct": 1.2, "oos_max_drawdown": 0.05, "round_trips": 100, "stop_hits": 0}'::jsonb,
				        NOW())
			`, sym, tf); err != nil {
				t.Fatalf("seed backtest job %s %s: %v", sym, tf, err)
			}
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol IN ($1, $2)`, symbol, symbol11)
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'FLEET10%'`)
	})

	// Nine 4x fillers carrying $8 tranche-1 halves each: Σ stored = $72 —
	// exactly the prod fleet shape (200×4×2%/2 per bot).
	for i := 0; i < 9; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, max_loss_usdt
			) VALUES (
				$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
				90, 110, 10, 4, 100,
				100, 100, 8
			)
		`, settings.ID, fmt.Sprintf("FLEET10FILL%d_USDT_PERP", i)); err != nil {
			t.Fatalf("insert filler bot %d: %v", i, err)
		}
	}

	// Tenth deploy: envelope 72 + full stop 8 = 80 = 0.8×$100 — at the
	// ceiling, must PASS (strict inequality).
	scanID := seedFleetScan(t, pool, symbol)
	if err := worker.deployPaper(ctx, *settings, scanID, false); err != nil {
		t.Fatalf("deployPaper tenth: %v", err)
	}
	var running int
	var storedStop decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE((SELECT max_loss_usdt FROM paper_grid_bots
			WHERE symbol = $1 AND status = 'RUNNING'), 0)
		FROM paper_grid_bots WHERE status = 'RUNNING'
	`, symbol).Scan(&running, &storedStop); err != nil {
		t.Fatalf("count fleet after tenth: %v", err)
	}
	if running != 10 {
		t.Fatalf("the tenth deploy must pass at the $80 ceiling, got %d running", running)
	}
	if !storedStop.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("tranche-1 storage must keep the $4 half, got %s", storedStop)
	}

	// Eleventh: the envelope itself must refuse it — Σ stored is now 76 and
	// 76 + 8 = 84 > 80. Pinned on the gate directly: the deploy loop's
	// portfolio cap (10/10) fires earlier with the same verdict (skip).
	if verdict := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, decimal.NewFromInt(8)); verdict == "" {
		t.Fatalf("the eleventh deploy must be envelope-refused: 76 + 8 = 84 > 0.8×100")
	}
	scanID2 := seedFleetScan(t, pool, symbol11)
	if err := worker.deployPaper(ctx, *settings, scanID2, false); err != nil {
		t.Fatalf("deployPaper eleventh: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE status = 'RUNNING'
	`).Scan(&running); err != nil {
		t.Fatalf("count fleet after eleventh: %v", err)
	}
	if running != 10 {
		t.Fatalf("the eleventh deploy must be skipped, got %d running", running)
	}
	var decision, rejection string
	if err := pool.QueryRow(ctx, `
		SELECT decision, COALESCE(rejection_reason, '') FROM autogrid_candidates
		WHERE scan_id = $1 AND symbol = $2
	`, scanID2, symbol11).Scan(&decision, &rejection); err != nil {
		t.Fatalf("load eleventh candidate: %v", err)
	}
	if decision != "REJECTED" {
		t.Fatalf("the eleventh candidate must be rejected (portfolio 10/10 or envelope), got %s / %q", decision, rejection)
	}
}

// TestDeployStopEnvelopeParityPaperReal pins the PAPER/REAL parity of the
// deploy envelope gate: the joint sum counts paper_grid_bots AND grid_bots
// as one risk account, so the same Σ stored stops yields the same verdict
// whether they sit in the paper table, the REAL table, or both.
func TestDeployStopEnvelopeParityPaperReal(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, _, live := newCooldownTestWorker(t, pool)
	// Breaker 50 → envelope limit 40; a $8 candidate reserve against Σ36
	// stored stops overflows (44), against Σ27 it fits (35).
	pinBreakerFixture(t, pool, live.ID, 11, "50")
	settings := *mustSettings(t, worker.service, live.ID)

	var accountID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO pionex_accounts (name, api_key_encrypted, api_secret_encrypted, is_enabled, is_paper)
		VALUES ('breaker-parity-test', 'x', 'y', TRUE, TRUE)
		RETURNING id
	`).Scan(&accountID); err != nil {
		t.Fatalf("create parity account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, accountID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, accountID)
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'PARITY%'`)
	})

	insertPaper := func(symbol string, stop int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, max_loss_usdt
			) VALUES ($1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC', 90, 110, 10, 2, 100, 100, 100, $3)
		`, settings.ID, symbol, stop); err != nil {
			t.Fatalf("insert paper filler %s: %v", symbol, err)
		}
	}
	insertReal := func(symbol string, stop int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO grid_bots (
				account_id, symbol, bu_order_id, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				request_fingerprint, autogrid_settings_id, max_loss_usdt
			) VALUES (
				$1, $2, md5(random()::text), 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
				90, 110, 10, 2, 100, md5(random()::text), $3, $4
			)
		`, accountID, symbol, settings.ID, stop); err != nil {
			t.Fatalf("insert real filler %s: %v", symbol, err)
		}
	}
	clearFleet := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'PARITY%'`); err != nil {
			t.Fatalf("clear paper fillers: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, accountID); err != nil {
			t.Fatalf("clear real fillers: %v", err)
		}
	}

	candidateStop := decimal.NewFromInt(8)

	// Σ36 paper-only: refused.
	for i := 0; i < 4; i++ {
		insertPaper(fmt.Sprintf("PARITYP%d_USDT_PERP", i), 9)
	}
	paperVerdict := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateStop)
	if paperVerdict == "" {
		t.Fatalf("Σ36 paper + $8 reserve must be refused (44 > 0.8×50)")
	}

	// Σ36 real-only: identical verdict string — the gate is mode-blind.
	clearFleet()
	for i := 0; i < 4; i++ {
		insertReal(fmt.Sprintf("PARITYR%d_USDT_PERP", i), 9)
	}
	realVerdict := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateStop)
	if realVerdict == "" {
		t.Fatalf("Σ36 real + $8 reserve must be refused (44 > 0.8×50)")
	}
	if realVerdict != paperVerdict {
		t.Fatalf("parity: same Σ stops must yield the same verdict, paper %q vs real %q", paperVerdict, realVerdict)
	}

	// Σ36 split 18+18 across BOTH tables: the joint sum still sees all 36.
	clearFleet()
	for i := 0; i < 2; i++ {
		insertPaper(fmt.Sprintf("PARITYMIXP%d_USDT_PERP", i), 9)
		insertReal(fmt.Sprintf("PARITYMIXR%d_USDT_PERP", i), 9)
	}
	mixedVerdict := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateStop)
	if mixedVerdict != paperVerdict {
		t.Fatalf("parity: mixed Σ36 must equal the single-table verdict, got %q vs %q", mixedVerdict, paperVerdict)
	}

	// Σ27 real-only fits (27+8 = 35 ≤ 40): the pass verdict is mode-blind too.
	clearFleet()
	for i := 0; i < 3; i++ {
		insertReal(fmt.Sprintf("PARITYOK%d_USDT_PERP", i), 9)
	}
	if verdict := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateStop); verdict != "" {
		t.Fatalf("Σ27 real + $8 reserve must pass, got %q", verdict)
	}
}
