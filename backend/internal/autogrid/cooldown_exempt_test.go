package autogrid

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCascadeFlipCooldownExemptReal (audit-2 P1 #3) pins the REAL lane of
// the cascade-short flip cooldown exemption (cooldown.go):
//
//   - no DIRECTION_FLIP proof in bot_execution_events  -> false (fail-closed);
//   - a REAL DIRECTION_FLIP event within 24h and no non-flip protective
//     close on the account/symbol                         -> true;
//   - the same flip plus ONE non-flip-family protective close
//     (STOP_LOSS, STOPPED, within 24h)                    -> false — the
//     exemption only disarms cooldowns armed solely by the flip family;
//   - a flip event recorded by the PAPER arm only         -> false — the
//     v2.0.142 filter: flipEventWithin24h must consult bot_source='REAL'.
func TestCascadeFlipCooldownExemptReal(t *testing.T) {
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
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	var accountID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO pionex_accounts (name, api_key_encrypted, api_secret_encrypted, is_enabled, is_paper)
		VALUES ('cooldown-exempt-real-test', 'x', 'y', TRUE, TRUE)
		RETURNING id
	`).Scan(&accountID); err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol LIKE 'CDX_REAL_%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, accountID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, accountID)
	})

	// A protective close on the REAL ledger: STOPPED + a non-exempt,
	// non-flip-family reason, closed within the 24h window.
	seedRealClose := func(t *testing.T, symbol string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO grid_bots (
				account_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				request_fingerprint, closed_reason, closed_at
			) VALUES (
				$1, $2, 'STOPPED', 'NEUTRAL', 'ARITHMETIC',
				90, 110, 10, 2, 100,
				$3, 'STOP_LOSS', NOW()
			)
		`, accountID, symbol, fmt.Sprintf("cdx-real-%d", time.Now().UnixNano())); err != nil {
			t.Fatalf("seed real close %s: %v", symbol, err)
		}
	}

	// (a) Zero events: no flip proof anywhere — fail-closed.
	if worker.cascadeFlipCooldownExemptReal(ctx, accountID, "CDX_REAL_A_USDT") {
		t.Fatalf("REAL: no flip event must NOT exempt the cooldown")
	}

	// (b) A REAL DIRECTION_FLIP within 24h, no protective closes at all.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_execution_events (bot_id, bot_number, bot_source, symbol, event_type, created_at)
		VALUES ('cdx-itest-real', 1, 'REAL', 'CDX_REAL_B_USDT', 'DIRECTION_FLIP', NOW())
	`); err != nil {
		t.Fatalf("seed REAL flip: %v", err)
	}
	if !worker.cascadeFlipCooldownExemptReal(ctx, accountID, "CDX_REAL_B_USDT") {
		t.Fatalf("REAL: a REAL flip with zero protective closes must exempt the cooldown")
	}

	// (c) The same flip plus one non-flip-family protective close: the
	// cooldown was NOT armed solely by the flip family — no exemption.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_execution_events (bot_id, bot_number, bot_source, symbol, event_type, created_at)
		VALUES ('cdx-itest-real', 1, 'REAL', 'CDX_REAL_C_USDT', 'DIRECTION_FLIP', NOW())
	`); err != nil {
		t.Fatalf("seed REAL flip (c): %v", err)
	}
	seedRealClose(t, "CDX_REAL_C_USDT")
	if worker.cascadeFlipCooldownExemptReal(ctx, accountID, "CDX_REAL_C_USDT") {
		t.Fatalf("REAL: a flip plus a STOP_LOSS close must NOT exempt the cooldown")
	}

	// (d) Only a PAPER flip event exists: the v2.0.142 bot_source='REAL'
	// filter must refuse it as flip proof for the REAL lane.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_execution_events (bot_id, bot_number, bot_source, symbol, event_type, created_at)
		VALUES ('cdx-itest-paper', 1, 'PAPER', 'CDX_REAL_D_USDT', 'DIRECTION_FLIP', NOW())
	`); err != nil {
		t.Fatalf("seed PAPER flip: %v", err)
	}
	if worker.cascadeFlipCooldownExemptReal(ctx, accountID, "CDX_REAL_D_USDT") {
		t.Fatalf("REAL: a PAPER-source flip must NOT exempt the REAL cooldown")
	}
}

// TestCascadeFlipCooldownExemptPaper pins the PAPER lane of the same
// exemption. Documented semantics: paper exists to train the exact pipeline
// REAL rides, so a REAL flip exempts the paper cooldown too — while any
// paper COMPLETED close outside the flip/exempt families re-arms it.
func TestCascadeFlipCooldownExemptPaper(t *testing.T) {
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
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol LIKE 'CDX_PAP_%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE settings_id = $1 AND symbol LIKE 'CDX_PAP_%'`, settings.ID)
	})

	// (a) Zero events: fail-closed, no exemption.
	if worker.cascadeFlipCooldownExemptPaper(ctx, settings.ID, "CDX_PAP_A_USDT") {
		t.Fatalf("PAPER: no flip event must NOT exempt the cooldown")
	}

	// (b) A REAL flip event + no paper protective closes: paper trains the
	// REAL pipeline, so the REAL flip exempts the paper cooldown.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_execution_events (bot_id, bot_number, bot_source, symbol, event_type, created_at)
		VALUES ('cdx-itest-real', 1, 'REAL', 'CDX_PAP_B_USDT', 'DIRECTION_FLIP', NOW())
	`); err != nil {
		t.Fatalf("seed REAL flip: %v", err)
	}
	if !worker.cascadeFlipCooldownExemptPaper(ctx, settings.ID, "CDX_PAP_B_USDT") {
		t.Fatalf("PAPER: a REAL flip with zero paper protective closes must exempt the cooldown")
	}

	// (c) The same REAL flip plus a paper COMPLETED non-flip close: the
	// cooldown was armed by a non-flip family member — no exemption.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_execution_events (bot_id, bot_number, bot_source, symbol, event_type, created_at)
		VALUES ('cdx-itest-real', 1, 'REAL', 'CDX_PAP_C_USDT', 'DIRECTION_FLIP', NOW())
	`); err != nil {
		t.Fatalf("seed REAL flip (c): %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, closed_reason, closed_at
		) VALUES (
			$1, 'CDX_PAP_C_USDT', 'COMPLETED', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 'STOP_LOSS', NOW()
		)
	`, settings.ID); err != nil {
		t.Fatalf("seed paper close: %v", err)
	}
	if worker.cascadeFlipCooldownExemptPaper(ctx, settings.ID, "CDX_PAP_C_USDT") {
		t.Fatalf("PAPER: a flip plus a COMPLETED STOP_LOSS close must NOT exempt the cooldown")
	}
}
