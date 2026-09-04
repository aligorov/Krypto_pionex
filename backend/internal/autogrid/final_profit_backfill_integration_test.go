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

// finishedListMock refuses the order-detail endpoint for finished grids and
// serves a configurable finished-bot list — the exact production shape the
// final-profit backfill reads.
type finishedListMock struct {
	server *httptest.Server
	mu     sync.Mutex
	orders []map[string]any
}

func (m *finishedListMock) setOrders(orders []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orders = orders
}

func newFinishedListMock(t *testing.T) *finishedListMock {
	t.Helper()
	mock := &finishedListMock{}
	mux := http.NewServeMux()
	// The detail endpoint refuses finished grids (v2.0.74 production fact).
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": false, "timestamp": time.Now().UnixMilli(),
			"code": "P_BOT_ORDER_NOT_FOUND", "message": "order not found or already closed",
		})
	})
	mux.HandleFunc("GET /api/v1/bot/orders", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		orders := mock.orders
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"orders": orders},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

// seedStoppedRealBot lands one terminal grid bot in the exact state the
// v2.0.74 chain left behind: settled by the terminal path, realized carrying
// the grid-only fallback figure, no finalProfitSource marker.
func seedStoppedRealBot(t *testing.T, pool *pgxpool.Pool, accountID, settingsID, symbol, buOrderID string) string {
	t.Helper()
	var botID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			realized_pnl_usdt, unrealized_pnl_usdt, closed_reason,
			closed_at, created_at, bot_number
		) VALUES (
			$1, $2, $3, 'STOPPED', 'NEUTRAL',
			'ARITHMETIC', 55.34, 57.52, 6, 2,
			100, 0, $4, 'REAL', 'REMOTE_TERMINAL_CONFIRMED', $5,
			0.21770320, 0, 'STOP_LOSS_NATIVE',
			NOW() - INTERVAL '3 hours', NOW() - INTERVAL '12 hours', 672
		)
		RETURNING id
	`, accountID, settingsID, symbol,
		"itest-"+time.Now().Format("150405.000000000"), buOrderID).Scan(&botID); err != nil {
		t.Fatalf("seed stopped bot %s: %v", symbol, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM grid_bots WHERE id = $1`, botID)
	})
	return botID
}

// TestFinalProfitBackfillRewritesGridOnlyStops re-settles the two production
// shapes through the 48h backfill loop:
//
//	XMR-class: finished record carries the FULL total (totalProfit −2.5, no
//	           profitExited) → the stored +0.2177 lie becomes −2.5.
//	JTO-class: finished record carries ONLY grid profit while inventory was
//	           closed on a loss stop → the positive grid figure is REFUSED:
//	           realized goes NULL (better empty than a lie) and the refusal
//	           is marked on the row.
func TestFinalProfitBackfillRewritesGridOnlyStops(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newFinishedListMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-finalpnl-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-finalpnl-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})
	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	var savedAccountID *string
	var savedStatus, savedMode string
	if err := pool.QueryRow(ctx, `
		SELECT account_id, status, execution_mode FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID, &savedStatus, &savedMode); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE autogrid_settings SET account_id = $2, status = $3, execution_mode = $4
			WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID, savedStatus, savedMode)
	})
	_, _ = pool.Exec(ctx, `
		UPDATE autogrid_settings SET account_id = $2, status = 'STOPPED', execution_mode = 'REAL'
		WHERE scope_key = $1::VARCHAR
	`, DefaultScope, account.ID)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	rec := &recordingHandler{}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	xmrID := seedStoppedRealBot(t, pool, account.ID, settings.ID, "XMRX_USDT_PERP", "FIN-XMR-1")
	jtoID := seedStoppedRealBot(t, pool, account.ID, settings.ID, "JTOX_USDT_PERP", "FIN-JTO-1")

	// The manage pass early-returns when NO bot is RUNNING — a helper bot
	// keeps the loop (and therefore the 48h backfill behind it) alive. The
	// detail endpoint refuses it too, so it settles through the not-found
	// branch and never touches the two rows under test.
	seedStoppedRealBotHelper := func() {
		t.Helper()
		helperBU := fmt.Sprintf("FIN-HELP-%d", time.Now().UnixNano())
		if _, err := pool.Exec(ctx, `
			INSERT INTO grid_bots (
				account_id, autogrid_settings_id, symbol, status, direction,
				grid_type, lower_price, upper_price, grid_num, leverage,
				quote_investment, extra_margin, request_fingerprint,
				execution_mode, reconciliation_state, bu_order_id, bot_number
			) VALUES (
				$1, $2, 'FINHELP_USDT_PERP', 'RUNNING', 'NEUTRAL',
				'ARITHMETIC', 90, 110, 20, 2,
				100, 0, $3, 'REAL', 'REMOTE_ID_PERSISTED', $4, 700
			)
		`, account.ID, settings.ID, "itest-"+time.Now().Format("150405.000000000"), helperBU); err != nil {
			t.Fatalf("seed helper bot: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1 AND symbol = 'FINHELP_USDT_PERP'`, account.ID)
		})
	}
	seedStoppedRealBotHelper()

	// XMR-class record: the full total rides the totalProfit alias; JTO-class
	// record: grid-only with closed inventory — the exact +0.2688 lie shape.
	mock.setOrders([]map[string]any{
		{
			"buOrderId": "FIN-XMR-1", "buOrderType": "futures_grid", "status": "finished",
			"base": "XMRX", "quote": "USDT", "closeTime": time.Now().UnixMilli(),
			"buOrderData": map[string]any{
				"status": "canceled", "reasonBy": "loss_stop",
				"profitExited": "0", "profitReduce": "0.2177032",
				"closedBaseAmount": "1.4", "fundingFeePayment": "0",
			},
		},
		{
			"buOrderId": "FIN-JTO-1", "buOrderType": "futures_grid", "status": "finished",
			"base": "JTOX", "quote": "USDT", "closeTime": time.Now().UnixMilli(),
			"buOrderData": map[string]any{
				"status": "canceled", "reasonBy": "loss_stop",
				"totalProfit": "-2.5", "profitReduce": "0.26879493",
				"closedBaseAmount": "1.2", "fundingFeePayment": "0",
			},
		},
	})

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}

	// XMR-class: grid-only positive on a loss stop with closed inventory →
	// REFUSED, realized NULL, refusal marked and warned.
	var xmrRealized *decimal.Decimal
	var xmrSource string
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt, COALESCE(model_state->>'finalProfitSource','')
		FROM grid_bots WHERE id = $1
	`, xmrID).Scan(&xmrRealized, &xmrSource); err != nil {
		t.Fatalf("load XMR-class bot: %v", err)
	}
	if xmrRealized != nil {
		t.Fatalf("the grid-only positive on a loss stop must be refused to NULL, got %s; logs: %s", *xmrRealized, rec.joined())
	}
	if xmrSource != "refused_"+string(pionex.FinalProfitGridResidual) {
		t.Fatalf("refusal must be marked on the row, got %q", xmrSource)
	}
	if !rec.contains("backfill settle refused") {
		t.Fatalf("refusal must Warn, logs: %s", rec.joined())
	}

	// JTO-class: the full total from the alias → −2.5 replaces the lie.
	var jtoRealized decimal.Decimal
	var jtoSource string
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt, COALESCE(model_state->>'finalProfitSource','')
		FROM grid_bots WHERE id = $1
	`, jtoID).Scan(&jtoRealized, &jtoSource); err != nil {
		t.Fatalf("load JTO-class bot: %v", err)
	}
	if !jtoRealized.Equal(decimal.NewFromFloat(-2.5)) {
		t.Fatalf("the full total must settle −2.5, got %s", jtoRealized)
	}
	if jtoSource != string(pionex.FinalProfitTotalAlias) {
		t.Fatalf("settle must record its source, got %q", jtoSource)
	}

	// Exactly-once: the finalProfitSource marker keeps settled rows out of
	// the next backfill selection — for refused rows too (NULL realized must
	// not re-arm the fetch loop).
	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	var stillSelected int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE id IN ($1, $2)
		  AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
		  AND model_state->>'finalProfitSource' IS NULL
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '48 hours'
	`, xmrID, jtoID).Scan(&stillSelected); err != nil {
		t.Fatalf("count re-selected: %v", err)
	}
	if stillSelected != 0 {
		t.Fatalf("marked rows must not re-enter the backfill, got %d", stillSelected)
	}
}
