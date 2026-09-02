package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newCooldownTestWorker(t *testing.T, pool *pgxpool.Pool) (*Worker, *Service, Settings) {
	t.Helper()
	ctx := context.Background()
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	// v2.0.27 routes paper deploys through the risk engine; migration 0001
	// seeds risk_settings with the kill switch ON — disable it or every
	// paper deploy in tests is blocked.
	if _, err := pool.Exec(ctx, `UPDATE risk_settings SET kill_switch_enabled = false WHERE id = 1`); err != nil {
		t.Fatalf("disable kill switch: %v", err)
	}
	return worker, service, *settings
}

// TestDeployPaperCooldownAfterProtectiveClose pins the redeploy cooldown to
// EVERY protective close, not just STOP_LOSS: a symbol that died by structural
// invalidation or a range break must stay cold for 2 hours (prod 2026-08-17:
// MMT stopped STRUCT_INVALID at 08:44 and was redeployed at 09:01), while a
// take-profit close may redeploy immediately.
func TestDeployPaperCooldownAfterProtectiveClose(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, _, settings := newCooldownTestWorker(t, pool)
	// v2.0.12 added revalidateFreshPrice to deployPaper (fail-closed on an
	// unreadable price). The fake symbol has no live ticker, so round 2 was
	// silently skipped and the test has been red since — point the worker's
	// public client at a stub ticker server pinned to the candidate price.
	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "COOLDOWN_USDT_PERP", "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	const symbol = "COOLDOWN_USDT_PERP"
	// Passing walk-forward verdicts so the backtest gate (applied to paper
	// since this release) never interferes with the cooldown assertions.
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
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup backtest jobs: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, closed_reason, closed_at
		) VALUES (
			$1, $2, 'COMPLETED', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 95, 'STRUCT_INVALID_ANTI_HUNT', NOW() - INTERVAL '10 minutes'
		)
	`, settings.ID, symbol); err != nil {
		t.Fatalf("insert closed paper bot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup paper bot: %v", err)
		}
	})

	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
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
	var candidateID string
	if err := pool.QueryRow(ctx, `
		SELECT id FROM autogrid_candidates WHERE scan_id = $1 AND symbol = $2
	`, scanID, symbol).Scan(&candidateID); err != nil {
		t.Fatalf("fetch candidate id: %v", err)
	}

	// Round 1: the structural close 10 minutes ago must block the redeploy.
	if err := worker.deployPaper(ctx, settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 1: %v", err)
	}
	var running int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != 0 {
		t.Fatalf("STRUCT_INVALID close must cool the symbol down for 2h — a new bot was deployed anyway")
	}

	// Round 2: same history, but closed in profit — redeploy is allowed.
	// v2.0.19: late gates persist rejectCandidate on the SAME row, and a
	// cooldown-time reject (round 1) marks this scan's candidate REJECTED —
	// matching production, the next scan mints a FRESH candidate row, so
	// round 2 deploys from a new scan run.
	if _, err := pool.Exec(ctx, `
		UPDATE paper_grid_bots SET closed_reason = 'TAKE_PROFIT'
		WHERE symbol = $1 AND status = 'COMPLETED'
	`, symbol); err != nil {
		t.Fatalf("flip close reason: %v", err)
	}
	var scanID2 string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID2); err != nil {
		t.Fatalf("insert scan run round 2: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, current_price, lower_price, upper_price,
			grid_num, recommended_trend, model_assumptions
		) VALUES (
			$1, $2, 'ACCEPTED', 100, 90, 110,
			10, 'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		)
	`, scanID2, symbol); err != nil {
		t.Fatalf("insert candidate round 2: %v", err)
	}
	if err := worker.deployPaper(ctx, settings, scanID2, false); err != nil {
		t.Fatalf("deployPaper round 2: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil {
		t.Fatalf("count running round 2: %v", err)
	}
	if running != 1 {
		t.Fatalf("TAKE_PROFIT close must not block redeploy, got %d running bots", running)
	}
	// The cooldown-time rejection must be visible on the round-1 candidate.
	var round1Decision, round1Reason string
	if err := pool.QueryRow(ctx, `
		SELECT decision, COALESCE(rejection_reason, '') FROM autogrid_candidates WHERE id = $1
	`, candidateID).Scan(&round1Decision, &round1Reason); err == nil {
		if round1Decision != "REJECTED" || round1Reason == "" {
			t.Fatalf("cooldown skip must persist a rejection reason, got %q / %q", round1Decision, round1Reason)
		}
	}
}

// TestDeployPaperStopEnvelopeGate pins the deploy-path fleet stop envelope:
// when the RUNNING fleet's max_loss sum plus the candidate's tranche-1 stop
// would exceed 0.8× the daily-loss breaker, the deploy must be refused with
// an honest rejection reason — until now only the tranche-2 top-up was
// gated, so a fresh deploy could widen the synchronized stop wave unchecked.
func TestDeployPaperStopEnvelopeGate(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, service, liveSettings := newCooldownTestWorker(t, pool)
	// Pin the numbers the gate reads. Breakers are chosen for determinism
	// against whatever the shared DB fleet carries: round 1 uses 1 USDT
	// (limit 0.8 — any candidate stop overflows it), round 2 uses 100000
	// (the envelope can never reach it). The bot-count caps rise so the two
	// filler bots cannot trip the portfolio cap or the risk engine's active
	// limit (defaults 1 and 3 — both silently consume this test's slots).
	var savedMode, savedTarget, savedStop, savedBreaker string
	var savedTranche bool
	var savedMaxBots, savedMaxGridBots int
	if err := pool.QueryRow(ctx, `
		SELECT pnl_target_mode, pnl_target_usdt::TEXT, max_loss_usdt::TEXT, tranche_deploy_enabled,
		       max_active_bots
		FROM autogrid_settings WHERE id = $1
	`, liveSettings.ID).Scan(&savedMode, &savedTarget, &savedStop, &savedTranche, &savedMaxBots); err != nil {
		t.Fatalf("snapshot autogrid_settings: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT max_daily_loss_usd::TEXT, max_active_grid_bots FROM risk_settings WHERE id = 1
	`).Scan(&savedBreaker, &savedMaxGridBots); err != nil {
		t.Fatalf("snapshot risk_settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET pnl_target_mode = $1, pnl_target_usdt = $2::NUMERIC, max_loss_usdt = $3::NUMERIC,
			    tranche_deploy_enabled = $4, max_active_bots = $5
			WHERE id = $6
		`, savedMode, savedTarget, savedStop, savedTranche, savedMaxBots, liveSettings.ID); err != nil {
			t.Errorf("restore autogrid_settings: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE risk_settings
			SET max_daily_loss_usd = $1::NUMERIC, max_active_grid_bots = $2 WHERE id = 1
		`, savedBreaker, savedMaxGridBots); err != nil {
			t.Errorf("restore risk_settings: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET pnl_target_mode = 'STATIC', pnl_target_usdt = 6, max_loss_usdt = 8, max_active_bots = 10
		WHERE id = $1
	`, liveSettings.ID); err != nil {
		t.Fatalf("pin autogrid_settings stops: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings SET max_daily_loss_usd = 1, max_active_grid_bots = 10 WHERE id = 1
	`); err != nil {
		t.Fatalf("pin round-1 breaker: %v", err)
	}
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("reload pinned settings: %v", err)
	}

	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "ENVEL_USDT_PERP", "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	const symbol = "ENVEL_USDT_PERP"
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
	// Filler fleet: two RUNNING bots carrying a $4 stop each — the envelope
	// is a fleet sum, so fillers on other symbols must count.
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, max_loss_usdt
			) VALUES (
				$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
				90, 110, 10, 2, 200,
				100, 100, 4
			)
		`, settings.ID, fmt.Sprintf("ENVELFILL%d_USDT_PERP", i)); err != nil {
			t.Fatalf("insert filler bot %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup backtest jobs: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'ENVEL%_USDT_PERP'`); err != nil {
			t.Errorf("cleanup paper bots: %v", err)
		}
	})

	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
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

	// Round 1: breaker 1 → limit 0.8; fleet 8 + candidate stop (8 full or 4
	// tranche-1) overflows it — the deploy must be refused, with the
	// envelope as the visible reason.
	if err := worker.deployPaper(ctx, *settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 1: %v", err)
	}
	var running int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil {
		t.Fatalf("count running round 1: %v", err)
	}
	if running != 0 {
		t.Fatalf("filled stop envelope must refuse the deploy, got %d running bots", running)
	}
	var rejection string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(rejection_reason, '') FROM autogrid_candidates
		WHERE scan_id = $1 AND symbol = $2
	`, scanID, symbol).Scan(&rejection); err != nil {
		t.Fatalf("load candidate round 1: %v", err)
	}
	if !strings.Contains(rejection, "конверт стопов флота") {
		t.Fatalf("rejection must name the fleet stop envelope, got %q", rejection)
	}

	// Round 2: breaker 100000 → the same deploy passes and the bot lands.
	if _, err := pool.Exec(ctx, `UPDATE risk_settings SET max_daily_loss_usd = 100000 WHERE id = 1`); err != nil {
		t.Fatalf("raise breaker: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_candidates
		SET decision = 'ACCEPTED', rejection_reason = NULL
		WHERE scan_id = $1 AND symbol = $2
	`, scanID, symbol); err != nil {
		t.Fatalf("reset candidate: %v", err)
	}
	if err := worker.deployPaper(ctx, *settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 2: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil {
		t.Fatalf("count running round 2: %v", err)
	}
	if running != 1 {
		t.Fatalf("deploy under the envelope cap must proceed, got %d running bots", running)
	}
}

// TestPaperAdjustPersistsCountAndNotifies covers the range-shift accounting
// chain end to end: after an ADJUST_UP the persisted adjustments_count must
// reach the state API (UI «Сдвиги») and the Telegram template must render the
// real count instead of a literal {{adjustments_count}} placeholder.
func TestPaperAdjustPersistsCountAndNotifies(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	var savedEnabled, savedNotifyAdjust bool
	var savedToken, savedChat, savedTemplate string
	if err := pool.QueryRow(ctx, `
		SELECT enabled, notify_range_adjust, COALESCE(bot_token, ''), COALESCE(chat_id, ''),
		       COALESCE(template_range_adjust, '')
		FROM telegram_settings WHERE id = 1
	`).Scan(&savedEnabled, &savedNotifyAdjust, &savedToken, &savedChat, &savedTemplate); err != nil {
		t.Fatalf("load telegram settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			UPDATE telegram_settings
			SET enabled = $1, notify_range_adjust = $2, bot_token = $3, chat_id = $4,
			    template_range_adjust = $5
			WHERE id = 1
		`, savedEnabled, savedNotifyAdjust, savedToken, savedChat, savedTemplate); err != nil {
			t.Errorf("restore telegram settings: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		UPDATE telegram_settings
		SET enabled = true, bot_token = 'test-token', chat_id = '1',
		    notify_range_adjust = true,
		    template_range_adjust = '🔄 Сдвиг диапазона: Бот #{{bot_number}} {{symbol}} {{lower_price}}–{{upper_price}} Сдвигов: {{adjustments_count}}'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("enable telegram: %v", err)
	}

	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "ADJT_USDT_PERP", "close": "115", "open": "100",
						"high": "116", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)

	worker, service, settings := newCooldownTestWorker(t, pool)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	const symbol = "ADJT_USDT_PERP"
	var botID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt
		) VALUES (
			$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 999, -999
		)
		RETURNING id
	`, settings.ID, symbol).Scan(&botID); err != nil {
		t.Fatalf("insert paper bot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup paper bot: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM notification_outbox WHERE payload::text LIKE '%' || $1 || '%'`, symbol); err != nil {
			t.Errorf("cleanup outbox: %v", err)
		}
	})

	if err := worker.managePaperBots(ctx, settings); err != nil {
		t.Fatalf("managePaperBots: %v", err)
	}

	var lower, upper string
	var adjustments int
	if err := pool.QueryRow(ctx, `
		SELECT lower_price::TEXT, upper_price::TEXT, COALESCE(adjustments_count, 0)
		FROM paper_grid_bots WHERE id = $1
	`, botID).Scan(&lower, &upper, &adjustments); err != nil {
		t.Fatalf("load bot after manage: %v", err)
	}
	if adjustments != 1 {
		t.Fatalf("adjustments_count must be 1 after the shift, got %d", adjustments)
	}
	if lowerF, _ := strconv.ParseFloat(lower, 64); lowerF != 105 {
		t.Fatalf("range must re-center on 115 with half-width 10 (lower 105), got %s", lower)
	}
	if upperF, _ := strconv.ParseFloat(upper, 64); upperF != 125 {
		t.Fatalf("range must re-center on 115 with half-width 10 (upper 125), got %s", upper)
	}

	active, err := service.listActiveBots(ctx, settings.ID)
	if err != nil {
		t.Fatalf("listActiveBots: %v", err)
	}
	for _, bot := range active {
		if bot.Symbol == symbol && bot.AdjustmentsCount != 1 {
			t.Fatalf("state API must expose adjustments_count for paper bots, got %d", bot.AdjustmentsCount)
		}
	}

	var payload string
	if err := pool.QueryRow(ctx, `
		SELECT payload::text FROM notification_outbox
		WHERE event_type = 'ADJUST_RANGE' AND payload::text LIKE '%' || $1 || '%'
		ORDER BY created_at DESC LIMIT 1
	`, symbol).Scan(&payload); err != nil {
		t.Fatalf("ADJUST_RANGE notification must be queued: %v", err)
	}
	if strings.Contains(payload, "{{adjustments_count}}") {
		t.Fatalf("telegram template must render the real count, got literal placeholder: %s", payload)
	}
	if !strings.Contains(payload, "Сдвигов: 1") {
		t.Fatalf("telegram notification must carry the new shift count, got: %s", payload)
	}
}

// TestPaperDeployRunsBacktestGate pins PAPER deployments to the same
// walk-forward exam REAL capital pays: a symbol without a verdict stays
// undeployed while jobs pend, a catastrophic traded-TF verdict rejects the
// candidate (prod: TUT walked into paper with 30M OOS −24.5% / DD 81%), and
// a passing verdict deploys. Pending neighbors never block.
func TestPaperDeployRunsBacktestGate(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	worker, _, settings := newCooldownTestWorker(t, pool)
	// The fake symbol has no live ticker on the real API, and the fresh-price
	// gate runs BEFORE the backtest gate — without a stub pinned to the
	// candidate price every round fail-closes as "stale" and the test never
	// reaches the gate it exists to pin (same disease the cooldown test had;
	// red since v2.0.12, surfaced by the 2026-08-20 postgres run).
	tickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": "BTGATE_USDT_PERP", "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")
	tradedTF := normalizeBacktestTF(settings.CandleInterval)
	allTFs := append([]string{tradedTF}, neighborBacktestTFs(tradedTF)...)

	const symbol = "BTGATE_USDT_PERP"
	for _, tf := range allTFs {
		if _, err := pool.Exec(ctx, `
			INSERT INTO backtest_jobs (symbol, interval, status, params)
			VALUES ($1, $2, 'QUEUED', '{}'::jsonb)
		`, symbol, tf); err != nil {
			t.Fatalf("seed queued job %s: %v", tf, err)
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup backtest jobs: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup paper bot: %v", err)
		}
	})

	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})
	var candidateID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, current_price, lower_price, upper_price,
			grid_num, recommended_trend, model_assumptions
		) VALUES (
			$1, $2, 'ACCEPTED', 100, 90, 110,
			10, 'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		)
		RETURNING id
	`, scanID, symbol).Scan(&candidateID); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}

	runningBots := func() int {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
		`, symbol).Scan(&count); err != nil {
			t.Fatalf("count running: %v", err)
		}
		return count
	}

	// Round 1: every TF still QUEUED — deployment must wait, not jump the gate.
	if err := worker.deployPaper(ctx, settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 1: %v", err)
	}
	if got := runningBots(); got != 0 {
		t.Fatalf("pending backtest must block paper deploy, got %d bots", got)
	}
	for _, tf := range allTFs {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM backtest_jobs WHERE symbol = $1 AND interval = $2
		`, symbol, tf).Scan(&count); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		if count != 1 {
			t.Fatalf("QUEUED job for %s must be reused, not duplicated (%d rows)", tf, count)
		}
	}

	// Round 2: catastrophic traded-TF verdict (the TUT numbers) — candidate
	// rejected, no bot.
	if _, err := pool.Exec(ctx, `
		UPDATE backtest_jobs
		SET status = 'DONE', finished_at = NOW(),
		    result = '{"folds": 4, "oos_return_pct": -24.52, "oos_max_drawdown": 0.8106, "round_trips": 4356, "stop_hits": 2}'::jsonb
		WHERE symbol = $1 AND interval = $2
	`, symbol, tradedTF); err != nil {
		t.Fatalf("finish traded job: %v", err)
	}
	if err := worker.deployPaper(ctx, settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 2: %v", err)
	}
	if got := runningBots(); got != 0 {
		t.Fatalf("catastrophic backtest must reject paper deploy, got %d bots", got)
	}
	var decision, rejection string
	if err := pool.QueryRow(ctx, `
		SELECT decision, COALESCE(rejection_reason, '') FROM autogrid_candidates WHERE id = $1
	`, candidateID).Scan(&decision, &rejection); err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if decision != "REJECTED" || !strings.Contains(rejection, "backtest gate") {
		t.Fatalf("candidate must be rejected by the gate, got %s / %q", decision, rejection)
	}

	// Round 3: passing traded-TF verdict — deploys even with pending neighbors.
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_candidates
		SET decision = 'ACCEPTED', rejection_reason = NULL WHERE id = $1
	`, candidateID); err != nil {
		t.Fatalf("reset candidate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE backtest_jobs
		SET result = '{"folds": 4, "oos_return_pct": 1.56, "oos_max_drawdown": 0.0183, "round_trips": 397, "stop_hits": 0}'::jsonb
		WHERE symbol = $1 AND interval = $2
	`, symbol, tradedTF); err != nil {
		t.Fatalf("pass traded job: %v", err)
	}
	if err := worker.deployPaper(ctx, settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 3: %v", err)
	}
	if got := runningBots(); got != 1 {
		t.Fatalf("passing backtest must allow paper deploy, got %d bots", got)
	}
}

// TestDeployPaperCooldownEscalation pins the v2.0.28 doubling window: two
// protective closes in 24h must block re-entry for 4h from the LATEST close
// (the old flat 2h window re-armed the same trend signal and the pair died
// again — prod VIRTUAL/NEAR double stops, CRWVX morning loop).
func TestDeployPaperCooldownEscalation(t *testing.T) {
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
					{"symbol": "ESCOOL_USDT_PERP", "close": "100", "open": "100",
						"high": "101", "low": "99", "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(tickers.Close)
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	const symbol = "ESCOOL_USDT_PERP"
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
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM backtest_jobs WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup backtest jobs: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol = $1`, symbol); err != nil {
			t.Errorf("cleanup paper bots: %v", err)
		}
	})
	// Two protective closes: the newest 3h ago, the older 6h ago. The flat
	// 2h window would ALLOW re-entry (3h > 2h); the escalated 4h window
	// (2 closes) must BLOCK.
	for _, age := range []string{"6 hours", "3 hours"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, closed_reason, closed_at
			) VALUES (
				$1, $2, 'COMPLETED', 'NEUTRAL', 'ARITHMETIC',
				90, 110, 10, 2, 200,
				100, 95, 'STOP_LOSS', NOW() - ($3)::interval
			)
		`, settings.ID, symbol, age); err != nil {
			t.Fatalf("insert closed bot (%s): %v", age, err)
		}
	}

	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
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

	// Round 1: 2 closes → 4h window, newest is 3h old → blocked.
	if err := worker.deployPaper(ctx, settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 1: %v", err)
	}
	var rejection string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(rejection_reason, '') FROM autogrid_candidates
		WHERE scan_id = $1 AND symbol = $2
	`, scanID, symbol).Scan(&rejection); err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if !strings.Contains(rejection, "окно 4h") {
		t.Fatalf("escalated cooldown must reject with a 4h window, got %q", rejection)
	}

	// Round 2: age the newest close beyond the 4h window → deploy proceeds.
	if _, err := pool.Exec(ctx, `
		UPDATE paper_grid_bots
		SET closed_at = NOW() - INTERVAL '5 hours'
		WHERE symbol = $1 AND closed_at > NOW() - INTERVAL '4 hours'
	`, symbol); err != nil {
		t.Fatalf("age newest close: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_candidates
		SET decision = 'ACCEPTED', rejection_reason = NULL
		WHERE scan_id = $1 AND symbol = $2
	`, scanID, symbol); err != nil {
		t.Fatalf("reset candidate: %v", err)
	}
	if err := worker.deployPaper(ctx, settings, scanID, false); err != nil {
		t.Fatalf("deployPaper round 2: %v", err)
	}
	var running int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol = $1 AND status = 'RUNNING'
	`, symbol).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != 1 {
		t.Fatalf("re-entry beyond the escalated window must deploy, got %d bots", running)
	}
}
