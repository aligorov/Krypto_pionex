package autogrid

import (
	"context"
	"encoding/json"
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

// blindExchangeMock emulates the signed grid endpoints with a PARTIALLY
// dead public price plane: indexes answer for an unrelated symbol only, so
// the map is healthy while both bots' symbols are missing — the delisting
// class where every supervised bot runs the v2.0.149 blind lane without the
// map itself failing. The order detail is keyed by buOrderId so each bot
// carries its own exchange total.
type blindExchangeMock struct {
	mu          sync.Mutex
	cancelCount int
	detail      map[string]map[string]any
	server      *httptest.Server
}

func newBlindExchangeMock(t *testing.T) *blindExchangeMock {
	t.Helper()
	mock := &blindExchangeMock{
		detail: map[string]map[string]any{
			// BLIND-A: exchange total −18.5 with 1.5 grid profit → the blind
			// floating estimate is −20 → total −18.5 breaches the $8 cap →
			// STOP_LOSS must fire with no local price at all.
			"BLIND-A": {
				"top": "120", "bottom": "100", "row": 20,
				"gridType": "arithmetic", "trend": "no_trend", "leverage": 1,
				"position": "0.5", "positionOpenPrice": "100",
				"profitReduce": "1.5", "profitWithdrawn": "0",
				"totalProfit": "-18.5",
				"riskStatus":  "NORMAL",
			},
			// BLIND-B: positive exchange total → the estimate is rejected
			// (fail-closed: never flatter the books) → unrealized stays 0,
			// the bot stays RUNNING, nothing is masked as a loss.
			"BLIND-B": {
				"top": "240", "bottom": "200", "row": 20,
				"gridType": "arithmetic", "trend": "no_trend", "leverage": 1,
				"position": "0.5", "positionOpenPrice": "200",
				"profitReduce": "1.5", "profitWithdrawn": "0",
				"totalProfit": "2.5",
				"riskStatus":  "NORMAL",
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("buOrderId")
		mock.mu.Lock()
		payload := mock.detail[id]
		mock.mu.Unlock()
		if payload == nil {
			t.Errorf("unexpected buOrderId %q", id)
			payload = map[string]any{}
		}
		bu := map[string]any{"status": "running", "reasonBy": ""}
		for k, v := range payload {
			bu[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": id,
				"status":    "running", "reasonBy": "",
				"buOrderData": bu,
			},
		})
	})
	// The handler above needs the detail INSIDE buOrderData — serve it
	// properly by rebuilding the response in one shot.
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		mock.cancelCount++
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("GET /uapi/v1/trade/fundingFee", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"fundings": []map[string]any{}},
		})
	})
	// Partial price-plane outage: indexes answer, but only for an unrelated
	// symbol — the map is healthy while both bots' symbols are missing (the
	// delisting/indexes-gap class; review P2-1 proved this exact shape must
	// still page and stop).
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": "OTHER_USDT_PERP", "indexPrice": "55", "markPrice": "55"},
			}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "price plane outage", http.StatusInternalServerError)
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *blindExchangeMock) cancels() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelCount
}

// TestBlindStopsIntegration proves the v2.0.149 blind lane end to end: with
// the price plane dead, the max-loss stop still fires off the exchange's
// own total (with the stopIntentTotal baseline and the blindness witness),
// a positive exchange total never fabricates a position, and the blindness
// episode is tracked on the worker.
func TestBlindStopsIntegration(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newBlindExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-blind-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-blind-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-blind-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-blind-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-blind-test%'`)
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
		if _, err := pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, status = $3, execution_mode = $4, ai_autotune_enabled = $5
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode, savedAutotune); err != nil {
			t.Errorf("restore settings failed — stand state may be dirty: %v", err)
		}
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

	botIDs := make(map[string]string)
	for _, spec := range []struct{ buID, lower, upper string }{
		{"BLIND-A", "100", "120"},
		{"BLIND-B", "200", "240"},
	} {
		var botID string
		err = pool.QueryRow(ctx, `
			INSERT INTO grid_bots (
				account_id, autogrid_settings_id, symbol, status, direction,
				grid_type, lower_price, upper_price, grid_num, leverage,
				quote_investment, extra_margin, request_fingerprint,
				execution_mode, reconciliation_state, bu_order_id,
				pnl_target_usdt, max_loss_usdt
			) VALUES (
				$1, $2, $3, 'RUNNING', 'NEUTRAL',
				'ARITHMETIC', $4, $5, 20, 1,
				100, 0, $6, 'REAL', 'REMOTE_ID_PERSISTED', $7,
				1000, 8
			)
			RETURNING id
		`, account.ID, settings.ID, spec.buID+"_USDT_PERP", spec.lower, spec.upper,
			"itest-"+time.Now().Format("150405.000000000"), spec.buID,
		).Scan(&botID)
		if err != nil {
			t.Fatalf("insert grid bot %s: %v", spec.buID, err)
		}
		botIDs[spec.buID] = botID
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("blind reconcile pass: %v", err)
	}

	// BLIND-A: the max-loss stop fired with no local price, on the
	// exchange-total basis, with the durable intent baseline stamped.
	var aStatus, aReason, aIntent, aUnrealized, aMarker string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason,''), COALESCE(model_state->>'stopIntentTotal',''),
		       unrealized_pnl_usdt::TEXT, COALESCE(model_state->>'priceFeedBlindFloating','')
		FROM grid_bots WHERE id = $1
	`, botIDs["BLIND-A"]).Scan(&aStatus, &aReason, &aIntent, &aUnrealized, &aMarker); err != nil {
		t.Fatalf("load BLIND-A: %v", err)
	}
	if aStatus != "STOP_REQUESTED" || aReason != "STOP_LOSS" {
		t.Fatalf("BLIND-A must stop blind on STOP_LOSS, got %s/%s", aStatus, aReason)
	}
	if got := decimal.RequireFromString(aIntent); !got.Equal(decimal.NewFromFloat(-18.5)) {
		t.Fatalf("BLIND-A stopIntentTotal = %s, want -18.5 (floor basis: 1.5 grid − 20 float)", aIntent)
	}
	if got := decimal.RequireFromString(aUnrealized); !got.Equal(decimal.NewFromInt(-20)) {
		t.Fatalf("BLIND-A unrealized = %s, want -20 (exchange-total estimate, not zero-masked)", aUnrealized)
	}
	if aMarker != "true" {
		t.Fatalf("BLIND-A must carry the priceFeedBlindFloating marker, got %q", aMarker)
	}
	if got := mock.cancels(); got != 1 {
		t.Fatalf("blind stop must submit exactly one native cancel, got %d", got)
	}
	var blindWitness string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(details->>'price_feed_blind','')
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'STOP_LOSS'
		ORDER BY created_at DESC LIMIT 1
	`, botIDs["BLIND-A"]).Scan(&blindWitness); err != nil || blindWitness != "true" {
		t.Fatalf("BLIND-A STOP_LOSS event must carry price_feed_blind=true (err=%v, got %q)", err, blindWitness)
	}

	// BLIND-B: a positive exchange total is not a position — no mask, no
	// stop, the bot keeps running.
	var bStatus, bIntent, bUnrealized, bMarker string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(model_state->>'stopIntentTotal',''),
		       unrealized_pnl_usdt::TEXT, COALESCE(model_state->>'priceFeedBlindFloating','')
		FROM grid_bots WHERE id = $1
	`, botIDs["BLIND-B"]).Scan(&bStatus, &bIntent, &bUnrealized, &bMarker); err != nil {
		t.Fatalf("load BLIND-B: %v", err)
	}
	if bStatus != "RUNNING" || bIntent != "" || bMarker != "" {
		t.Fatalf("BLIND-B must stay untouched RUNNING (got %s, intent %q, marker %q)", bStatus, bIntent, bMarker)
	}
	if got := decimal.RequireFromString(bUnrealized); !got.IsZero() {
		t.Fatalf("BLIND-B unrealized = %s, want 0 (positive estimate rejected)", bUnrealized)
	}

	// The blindness episode is tracked on the worker.
	if worker.priceFeedBlindSince == nil {
		t.Fatal("worker must track the price-feed blindness episode")
	}

	// Pass 2: the accepted cancel must not double-submit.
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("blind reconcile pass 2: %v", err)
	}
	if got := mock.cancels(); got != 1 {
		t.Fatalf("cancel must stay exactly-once after the remote-verify guard, got %d", got)
	}
	var bStatus2 string
	if err := pool.QueryRow(ctx, `SELECT status FROM grid_bots WHERE id = $1`,
		botIDs["BLIND-B"]).Scan(&bStatus2); err != nil || bStatus2 != "RUNNING" {
		t.Fatalf("BLIND-B must stay RUNNING across blind passes (err=%v, got %s)", err, bStatus2)
	}
}
