package autogrid

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestHealV103UnlockIdentity pins the v2.0.105 heal against a real database:
// it must reopen phantom unlock_identity finals (NULL + PENDING + archived
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

	worker := &Worker{
		db:       pool,
		service:  service,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	seed := func(t *testing.T, id, marker string, guarded bool) {
		t.Helper()
		state := `jsonb_build_object('finalProfitSource', '` + marker + `', 'finalUsdtExchange', '52.279854292')`
		if guarded {
			state += ` || jsonb_build_object('v103UnlockBug', '52.279854292')`
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO grid_bots (
				id, account_id, symbol, bu_order_id, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				request_fingerprint, autogrid_settings_id, realized_pnl_usdt,
				model_state, closed_reason, closed_at
			) VALUES (
				$1, (SELECT account_id FROM grid_bots LIMIT 1), 'HEALV103_USDT_PERP',
				$1, 'STOPPED', 'NEUTRAL', 'ARITHMETIC',
				1, 2, 4, 2, 100, md5(random()::text), $2, 52.279854292,
				`+state+`, 'GRID_AGED_HALF_LIFE', NOW()
			)
			ON CONFLICT (id) DO UPDATE
			SET model_state = EXCLUDED.model_state,
			    realized_pnl_usdt = EXCLUDED.realized_pnl_usdt,
			    status = 'STOPPED',
			    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED'
		`, id, settings.ID); err != nil {
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
	if realized != nil {
		t.Fatalf("phantom final must be NULL after heal, got %v", *realized)
	}
	if recon != TerminalFinalPendingExchange {
		t.Fatalf("phantom row must be reopened pending, got %s", recon)
	}
	if bugFig != "52.279854292" {
		t.Fatalf("buggy figure must be archived under v103UnlockBug, got %q", bugFig)
	}

	var guardRealized float64
	var guardRecon string
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt::FLOAT8, reconciliation_state
		FROM grid_bots WHERE id = $1
	`, healedID).Scan(&guardRealized, &guardRecon); err != nil {
		t.Fatalf("guarded row read: %v", err)
	}
	if guardRealized != 52.279854292 || guardRecon != "REMOTE_TERMINAL_CONFIRMED" {
		t.Fatalf("guarded row must stay untouched, got %v/%s", guardRealized, guardRecon)
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
