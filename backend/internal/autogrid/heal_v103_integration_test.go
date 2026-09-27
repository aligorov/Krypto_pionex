package autogrid

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestHealV103UnlockIdentity pins the v2.0.105 heal against a real database:
// it must reopen identified phantom finals (retained + PENDING + archived
// buggy figure), never touch rows that carry the v103UnlockBug guard, and
// never fail on placeholder arity — the v2.0.106 lesson (a lone $2 made the
// Exec error out on every boot while the operator watched the phantom TOTAL
// PnL card, the fifth strike of the pgx parameter class).
func TestHealV103UnlockIdentity(t *testing.T) {
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

	// grid_bots.account_id is NOT NULL: on a fresh disposable database the
	// default settings carry no account and no bot rows exist to borrow one
	// from, so the test owns a throwaway account (same shape the real-deploy
	// harnesses create) and detaches it on cleanup.
	accountService := accounts.NewService(pool)
	seedAccount, accErr := accountService.Create(ctx, accounts.CreateInput{
		Name:   "integration-heal-v103-" + time.Now().Format("150405.000000000"),
		APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if accErr != nil {
		t.Fatalf("create seed account: %v", accErr)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, seedAccount.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, seedAccount.ID)
	})

	worker := &Worker{
		db:      pool,
		service: service,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	seed := func(t *testing.T, id, marker string, guarded bool) {
		t.Helper()
		state := `jsonb_build_object('finalProfitSource', '` + marker + `', 'finalUsdtExchange', '52.279854292', 'unlockIdentityBasis', 'exchange_investment')`
		if guarded {
			state += ` || jsonb_build_object('v103UnlockBug', '52.279854292')`
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO grid_bots (
				id, account_id, symbol, bu_order_id, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				request_fingerprint, autogrid_settings_id, realized_pnl_usdt,
				model_state, closed_reason, closed_at, reconciliation_state
			) VALUES (
				$1::UUID, $3::UUID, 'HEALV103_USDT_PERP',
				$1::TEXT, 'STOPPED', 'NEUTRAL', 'ARITHMETIC',
				1, 2, 4, 2, 100, md5(random()::text), $2, 52.279854292,
				`+state+`, 'GRID_AGED_HALF_LIFE', NOW(), 'REMOTE_TERMINAL_CONFIRMED'
			)
			ON CONFLICT (id) DO UPDATE
			SET model_state = EXCLUDED.model_state,
			    realized_pnl_usdt = EXCLUDED.realized_pnl_usdt,
			    status = 'STOPPED',
			    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED'
		`, id, settings.ID, seedAccount.ID); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	const phantomID = "00000000-0000-4000-8000-000000000101"
	const healedID = "00000000-0000-4000-8000-000000000102"
	seed(t, phantomID, "unlock_identity", false)
	seed(t, healedID, "unlock_identity", true)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE id IN ($1, $2)`, phantomID, healedID)
	})

	// First run: heals the unguarded row, skips the guarded one.
	worker.healV103UnlockIdentity(ctx)

	var realized *float64
	var recon, bugFig string
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt::FLOAT8, reconciliation_state,
		       COALESCE(model_state->>'v103UnlockBug','')
		FROM grid_bots WHERE id = $1
	`, phantomID).Scan(&realized, &recon, &bugFig); err != nil {
		t.Fatalf("phantom row read: %v", err)
	}
	if realized == nil || *realized != 52.27985429 {
		t.Fatalf("previous final must be retained until replacement, got %v", realized)
	}
	if recon != TerminalFinalPendingExchange {
		t.Fatalf("phantom row must be reopened pending, got %s", recon)
	}
	if bugFig != "52.279854292" {
		t.Fatalf("buggy figure must be archived under v103UnlockBug, got %q", bugFig)
	}

	var guardExact bool
	var guardRecon string
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt = 52.27985429, reconciliation_state
		FROM grid_bots WHERE id = $1
	`, healedID).Scan(&guardExact, &guardRecon); err != nil {
		t.Fatalf("guarded row read: %v", err)
	}
	// Numeric equality in SQL: casting to FLOAT8 rounds the 9th digit and
	// made the untouched row compare unequal to itself.
	if !guardExact || guardRecon != "REMOTE_TERMINAL_CONFIRMED" {
		t.Fatalf("guarded row must stay untouched, got exact=%v/%s", guardExact, guardRecon)
	}

	// A fresh process (flag reset) must be a no-op for BOTH rows.
	worker.terminalIdentityHealDone = false
	worker.healV103UnlockIdentity(ctx)
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(model_state->>'finalProfitSource','')
		FROM grid_bots WHERE id = $1
	`, phantomID).Scan(&recon); err != nil || recon == "unlock_identity" {
		t.Fatalf("re-heal must not revert the fixed row: %v %s", err, recon)
	}
}
