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

// equityDetailMock serves GET /uapi/v1/account/detail with a mutable USDT
// row: assets (free+frozen), unrealizedPnL, available, debts.
type equityDetailMock struct {
	server *httptest.Server
	mu     sync.Mutex
	assets string
	unreal string
	avail  string
	debts  string
}

func newEquityDetailMock(t *testing.T) *equityDetailMock {
	t.Helper()
	mock := &equityDetailMock{assets: "500", unreal: "0", avail: "500", debts: "0"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		mock.mu.Lock()
		row := map[string]any{
			"coin": "USDT", "assets": mock.assets, "free": mock.avail,
			"frozen": "0", "available": mock.avail, "unrealizedPnL": mock.unreal,
			"totalInitialMargin": "0", "debts": mock.debts,
		}
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"balances": []map[string]any{row}},
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *equityDetailMock) set(t *testing.T, assets, unreal, avail string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assets, m.unreal, m.avail = assets, unreal, avail
}

// TestAccountEquitySnapshotEpoch proves the wallet-truth chain end to end:
// (1) the worker snapshots the futures wallet equity from
// /uapi/v1/account/detail, (2) the 5-minute durable throttle holds, (3) the
// epoch PnL (equity_now − first snapshot) is derivable through the service —
// the operator's headline number can no longer be read off fee-blind bot PnL
// fields.
func TestAccountEquitySnapshotEpoch(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newEquityDetailMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-equity-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL
		WHERE scope_key = 'default' AND account_id IN (
			SELECT id FROM pionex_accounts WHERE name LIKE 'integration-equity-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-equity-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM account_equity_snapshots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
	})

	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	var savedAccountID *string
	if err := pool.QueryRow(ctx, `
		SELECT account_id FROM autogrid_settings WHERE scope_key = 'default'
	`).Scan(&savedAccountID); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			UPDATE autogrid_settings SET account_id = $2 WHERE scope_key = $1::VARCHAR
		`, DefaultScope, savedAccountID)
	})
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings SET account_id = $2 WHERE scope_key = $1::VARCHAR
	`, DefaultScope, account.ID); err != nil {
		t.Fatalf("retarget settings: %v", err)
	}
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	// Anchor: $500 wallet, no floating PnL.
	worker.captureAccountEquity(ctx, *settings)
	var count int
	var equity, available string
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), MAX(equity_usdt)::TEXT, MAX(available_usdt)::TEXT
		FROM account_equity_snapshots WHERE account_id = $1
	`, account.ID).Scan(&count, &equity, &available); err != nil {
		t.Fatalf("load snapshots: %v", err)
	}
	if count != 1 || !decimal.RequireFromString(equity).Equal(decimal.NewFromInt(500)) ||
		!decimal.RequireFromString(available).Equal(decimal.NewFromInt(500)) {
		t.Fatalf("anchor snapshot wrong: n=%d equity=%s available=%s", count, equity, available)
	}

	// The durable 5-minute throttle: an immediate second capture must not
	// add a row even though the wallet moved.
	mock.set(t, "503", "0", "503")
	worker.captureAccountEquity(ctx, *settings)
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM account_equity_snapshots WHERE account_id = $1
	`, account.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("5-minute throttle violated: n=%d (%v)", count, err)
	}

	// Age the anchor past the throttle and move the wallet by −$2.60 (fees +
	// a stop the bot PnL fields never see): equity 497.40 via assets 500 +
	// unrealized −2.60 — the unrealized leg MUST be included.
	if _, err := pool.Exec(ctx, `
		UPDATE account_equity_snapshots
		SET captured_at = NOW() - INTERVAL '6 minutes'
		WHERE account_id = $1
	`, account.ID); err != nil {
		t.Fatalf("age anchor: %v", err)
	}
	mock.set(t, "500", "-2.6", "480")
	worker.captureAccountEquity(ctx, *settings)
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM account_equity_snapshots WHERE account_id = $1
	`, account.ID).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT equity_usdt::TEXT FROM account_equity_snapshots
		WHERE account_id = $1 ORDER BY captured_at DESC LIMIT 1
	`, account.ID).Scan(&equity); err != nil {
		t.Fatalf("load newest snapshot: %v", err)
	}
	if count != 2 {
		t.Fatalf("second snapshot must persist after the throttle window, got n=%d", count)
	}
	if got := decimal.RequireFromString(equity); !got.Equal(decimal.NewFromFloat(497.4)) {
		t.Fatalf("equity must net assets + unrealizedPnL (500 − 2.6), got %s", equity)
	}

	// The service-side epoch accounting: equity_now − first snapshot.
	summary, err := service.AccountEquityEpoch(ctx)
	if err != nil {
		t.Fatalf("epoch summary: %v", err)
	}
	if summary == nil {
		t.Fatal("epoch summary must exist after snapshots")
	}
	if !summary.EquityStartUSDT.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("epoch anchor must be the first snapshot (500), got %s", summary.EquityStartUSDT)
	}
	if !summary.EpochPnLUSDT.Equal(decimal.NewFromFloat(-2.6)) {
		t.Fatalf("epoch PnL (wallet truth) must be −2.60, got %s", summary.EpochPnLUSDT)
	}
	if summary.Snapshots != 2 {
		t.Fatalf("summary must count both snapshots, got %d", summary.Snapshots)
	}

	// An empty futures wallet is a silent no-op (fail-open), never an error
	// row or a zero-equity anchor.
	mock.set(t, "0", "0", "0")
	if _, err := pool.Exec(ctx, `
		UPDATE account_equity_snapshots
		SET captured_at = NOW() - INTERVAL '6 minutes'
		WHERE account_id = $1
	`, account.ID); err != nil {
		t.Fatalf("age snapshots: %v", err)
	}
	worker.captureAccountEquity(ctx, *settings)
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM account_equity_snapshots WHERE account_id = $1
	`, account.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("an empty wallet must add no snapshot, got n=%d (%v)", count, err)
	}
}
