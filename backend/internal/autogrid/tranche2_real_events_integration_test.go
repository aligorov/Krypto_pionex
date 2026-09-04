package autogrid

import (
	"context"
	"encoding/json"
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
	"github.com/shopspring/decimal"
)

// tranche2ExchangeMock serves everything one REAL manage pass touches for a
// tranche-2 top-up: order detail (running), indexes/tickers for prices, the
// signed funding feed (empty), and the invest_in adjustParams slot.
type tranche2ExchangeMock struct {
	server      *httptest.Server
	investCalls atomic.Int64
	lastBody    map[string]any
}

func newTranche2ExchangeMock(t *testing.T, symbol, price string) *tranche2ExchangeMock {
	t.Helper()
	mock := &tranche2ExchangeMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    "running", "reasonBy": "",
				"buOrderData": map[string]any{
					"status": "running", "reasonBy": "",
					"top": "110", "bottom": "90", "row": 20,
					"gridType": "arithmetic", "trend": "no_trend", "leverage": 2,
					"position": "0", "positionOpenPrice": "100",
					"profitReduce": "0.1", "profitWithdrawn": "0",
					"riskStatus": "NORMAL",
				},
			},
		})
	})
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": symbol, "indexPrice": price, "markPrice": price},
			}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": []map[string]any{
				{"symbol": symbol, "close": price, "open": price,
					"high": price, "low": price, "volume": "1000"},
			}},
		})
	})
	// Too few candles → regimeForSymbol returns "" → not trending → the 24h
	// time-box top-up path is reachable.
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": []map[string]any{
				{"time": time.Now().UnixMilli(), "open": price, "close": price,
					"high": price, "low": price, "volume": "1"},
			}},
		})
	})
	mux.HandleFunc("GET /uapi/v1/trade/fundingFee", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"fundings": []map[string]any{}},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParams", func(w http.ResponseWriter, r *http.Request) {
		mock.investCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	// The wallet-truth capture is fail-open: an empty detail must not disturb
	// the manage pass under test.
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"balances": []map[string]any{}},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// TestTranche2RealEvents proves the REAL invest_in top-up leaves the SAME
// durable trail the paper path always had: a TRANCHE_2 bot_execution_events
// row (bot_source=REAL, investment before/after, doubled effTarget/effStop)
// and a rendered Telegram outbox entry. v2.0.74 doubled real margin with
// zero ledger events — the operator learned nothing while bots changed size.
func TestTranche2RealEvents(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const symbol = "TRN2_USDT_PERP"
	mock := newTranche2ExchangeMock(t, symbol, "100")
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-tranche2-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-tranche2-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-tranche2-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-tranche2-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-tranche2-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup telemetry: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id IN (
			SELECT id::TEXT FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup events: %v", err)
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

	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	// The invest_in top-up must clear the durable exposure gates: pin the
	// limits high for the fixture and restore after (the tranche amounts in
	// play are far below any production symbol cap).
	var savedSymbolCap, savedAccountCap, savedDaily string
	if err := pool.QueryRow(ctx, `
		SELECT max_symbol_exposure_usd::TEXT, max_account_exposure_usd::TEXT, max_daily_loss_usd::TEXT
		FROM risk_settings WHERE id = 1
	`).Scan(&savedSymbolCap, &savedAccountCap, &savedDaily); err != nil {
		t.Fatalf("snapshot risk caps: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE risk_settings
			SET max_symbol_exposure_usd = $2::NUMERIC, max_account_exposure_usd = $3::NUMERIC,
			    max_daily_loss_usd = $4::NUMERIC
			WHERE id = 1
		`, savedSymbolCap, savedAccountCap, savedDaily)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = false,
		    max_symbol_exposure_usd = 100000, max_account_exposure_usd = 100000,
		    max_daily_loss_usd = 100000, max_leverage = 10
		WHERE id = 1
	`); err != nil {
		t.Fatalf("pin risk caps: %v", err)
	}

	var savedAccountID *string
	var savedStatus, savedMode string
	if err := pool.QueryRow(ctx, `
		SELECT account_id, status, execution_mode FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID, &savedStatus, &savedMode); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode); err != nil {
			t.Errorf("restore settings: %v", err)
		}
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

	// Telegram ON with the default range-adjust template so the outbox row
	// must render the real template (no raw {{...}} placeholders).
	tpl := "🔄 <b>Транш-2:</b> Бот #{{bot_number}} {{symbol}} [{{lower_price}} – {{upper_price}}], сдвигов {{adjustments_count}}"
	var telegramSaved struct {
		Enabled bool
		Token   string
		Chat    string
		Notify  bool
		Tmpl    *string
	}
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(enabled,false), COALESCE(bot_token,''), COALESCE(chat_id,''),
		       COALESCE(notify_range_adjust,false), template_range_adjust
		FROM telegram_settings WHERE id = 1
	`).Scan(&telegramSaved.Enabled, &telegramSaved.Token, &telegramSaved.Chat,
		&telegramSaved.Notify, &telegramSaved.Tmpl)
	t.Cleanup(func() {
		var restoreTmpl any
		if telegramSaved.Tmpl != nil {
			restoreTmpl = *telegramSaved.Tmpl
		}
		_, _ = pool.Exec(ctx, `
			UPDATE telegram_settings
			SET enabled = $1, bot_token = $2, chat_id = $3, notify_range_adjust = $4,
			    template_range_adjust = COALESCE($5::TEXT, template_range_adjust)
			WHERE id = 1
		`, telegramSaved.Enabled, telegramSaved.Token, telegramSaved.Chat, telegramSaved.Notify, restoreTmpl)
		_, _ = pool.Exec(ctx, `DELETE FROM notification_outbox WHERE event_type = 'TRANCHE_2'`)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE telegram_settings
		SET enabled = true, bot_token = 'test-token', chat_id = '100500',
		    notify_range_adjust = true, template_range_adjust = $1
		WHERE id = 1
	`, tpl); err != nil {
		t.Fatalf("enable telegram fixture: %v", err)
	}

	// One RUNNING REAL bot past the 24h time-box with a pending tranche-2:
	// investment 100 of base 200, targets halved at tranche-1.
	var botID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			pnl_target_usdt, max_loss_usdt, created_at, bot_number,
			model_state
		) VALUES (
			$1, $2, $3, 'RUNNING', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $4, 'REAL', 'REMOTE_ID_PERSISTED', $5,
			4.5, 2, NOW() - INTERVAL '25 hours', 777,
			'{"trancheDeployed": 1, "trancheBase": "200", "trancheEntry": "100", "atrPctEntry": 0.5}'::jsonb
		)
		RETURNING id
	`, account.ID, settings.ID, symbol,
		"itest-"+time.Now().Format("150405.000000000"), "TRN2-bu").Scan(&botID); err != nil {
		t.Fatalf("seed tranche bot: %v", err)
	}

	rec := &recordingHandler{}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	if got := mock.investCalls.Load(); got != 1 {
		t.Fatalf("expected exactly one invest_in, got %d; worker logs: %s", got, rec.joined())
	}
	if mock.lastBody["type"] != "invest_in" {
		t.Fatalf("adjust must be invest_in, got %v", mock.lastBody["type"])
	}

	var trancheDeployed int
	var investment, target, maxLoss decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       quote_investment, pnl_target_usdt, max_loss_usdt
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&trancheDeployed, &investment, &target, &maxLoss); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if trancheDeployed != 2 || !investment.Equal(decimal.NewFromInt(200)) ||
		!target.Equal(decimal.NewFromFloat(9)) || !maxLoss.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("tranche-2 must double margin and targets: tranche=%d inv=%s target=%s loss=%s",
			trancheDeployed, investment, target, maxLoss)
	}

	// The durable ledger event — v2.0.74's missing piece.
	var source, reason, invBefore, invAfter, effTarget, effStop string
	if err := pool.QueryRow(ctx, `
		SELECT bot_source, details->>'reason', details->>'investment_before',
		       details->>'investment_after', details->>'effective_target',
		       details->>'effective_max_loss'
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'TRANCHE_2'
		ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&source, &reason, &invBefore, &invAfter, &effTarget, &effStop); err != nil {
		t.Fatalf("TRANCHE_2 event must be logged for the REAL top-up: %v", err)
	}
	if source != "REAL" {
		t.Fatalf("event must carry bot_source=REAL, got %s", source)
	}
	if invBefore != "100" || invAfter != "200" {
		t.Fatalf("event must record investment before/after, got %s → %s", invBefore, invAfter)
	}
	if effTarget != "9.00" || effStop != "4.00" {
		t.Fatalf("event must record doubled effTarget/effStop, got %s/%s", effTarget, effStop)
	}
	if !strings.Contains(reason, "time-box") {
		t.Fatalf("event must carry the trigger reason, got %q", reason)
	}

	// The Telegram outbox entry — rendered, placeholder-free.
	var payload string
	if err := pool.QueryRow(ctx, `
		SELECT payload::TEXT FROM notification_outbox
		WHERE event_type = 'TRANCHE_2' ORDER BY created_at DESC LIMIT 1
	`).Scan(&payload); err != nil {
		t.Fatalf("TRANCHE_2 telegram outbox row missing: %v", err)
	}
	if strings.Contains(payload, "{{") {
		t.Fatalf("telegram payload must render every placeholder, got %s", payload)
	}
	if !strings.Contains(payload, "777") || !strings.Contains(payload, symbol) {
		t.Fatalf("telegram payload must address the bot, got %s", payload)
	}
}
