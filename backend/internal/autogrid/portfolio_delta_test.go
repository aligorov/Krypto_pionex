package autogrid

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// portfolioTestEnv wires a disposable account + the default settings + a bare
// worker against a real PostgreSQL. No exchange mock: every function under
// test here (calculateFleetNetDelta, marginReserveBlocker) is SQL-bound and
// never touches a client. The cleanup removes only this suite's rows — grid
// bots by seeded symbol prefix, paper bots likewise, snapshots and the
// account by id — so the shared default settings stay untouched for others.
type portfolioTestEnv struct {
	pool     *pgxpool.Pool
	service  *Service
	worker   *Worker
	account  *accounts.Account
	settings *Settings
}

func newPortfolioTestEnv(t *testing.T) *portfolioTestEnv {
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

	accountName := "integration-portfolio-test-" + time.Now().Format("150405.000000000")
	// Hermetic pre-clean: leftovers of a hard-crashed prior run of this same
	// suite would poison the fleet sums asserted below.
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE symbol LIKE 'PFD-%' AND autogrid_settings_id = (
		SELECT id FROM autogrid_settings WHERE scope_key = 'default')`)
	_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'PFDP-%' AND settings_id = (
		SELECT id FROM autogrid_settings WHERE scope_key = 'default')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-portfolio-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM account_equity_snapshots WHERE account_id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'PFDP-%' AND settings_id = (
			SELECT id FROM autogrid_settings WHERE scope_key = 'default')`)
	})

	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	return &portfolioTestEnv{
		pool: pool, service: service, worker: worker,
		account: account, settings: settings,
	}
}

// seedRealBot inserts one grid_bots row for the REAL delta query: RUNNING by
// default, unique request_fingerprint per the migration's unique index, and
// an explicit bu_order_id — the REAL query filters on it being NOT NULL.
func (env *portfolioTestEnv) seedRealBot(
	t *testing.T, symbol, direction, status, modelState, structContext string,
	investment decimal.Decimal, leverage int,
) string {
	t.Helper()
	var botID string
	err := env.pool.QueryRow(context.Background(), `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, bu_order_id,
			model_state, struct_context
		) VALUES (
			$1, $2, $3, $4, $5,
			'ARITHMETIC', 1.4, 1.8, 20, $6,
			$7, 0, $8, 'REAL', 'REMOTE_ID_PERSISTED', $9,
			$10::JSONB, NULLIF($11, '')::JSONB
		)
		RETURNING id
	`, env.account.ID, env.settings.ID, symbol, status, direction,
		leverage, investment,
		"itest-"+time.Now().Format("150405.000000000")+"-"+symbol,
		"PFD-"+symbol, modelState, structContext,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed real bot %s: %v", symbol, err)
	}
	return botID
}

// seedPaperBot inserts one paper_grid_bots row for the PAPER delta query
// (the paper arm of calculateFleetNetDelta reads mark_price/entry_price and
// model_state->shiftPosition, mirroring the REAL struct_context contract).
func (env *portfolioTestEnv) seedPaperBot(
	t *testing.T, symbol, direction, status, modelState string,
	investment decimal.Decimal, leverage int,
) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage,
			quote_investment, entry_price, mark_price, model_state
		) VALUES (
			$1, $2, $3, $4, 'ARITHMETIC',
			1.4, 1.8, 20, $5,
			$6, 1.5, 1.5, $7::JSONB
		)
	`, env.settings.ID, symbol, status, direction, leverage, investment, modelState); err != nil {
		t.Fatalf("seed paper bot %s: %v", symbol, err)
	}
}

// seedSnapshot writes one account_equity_snapshots row (v2.0.140 margin
// reserve reads exactly this shape: ORDER BY captured_at DESC LIMIT 1).
func (env *portfolioTestEnv) seedSnapshot(t *testing.T, equity decimal.Decimal, capturedAt time.Time) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO account_equity_snapshots
			(account_id, equity_usdt, assets_usdt, available_usdt, unrealized_pnl_usdt, source, captured_at)
		VALUES ($1, $2, $3, $4, 0, 'bot_aggregate', $5)
	`, env.account.ID, equity, equity, equity, capturedAt); err != nil {
		t.Fatalf("seed equity snapshot: %v", err)
	}
}

// TestCalculateFleetNetDeltaNeutralPark locks the v2.0.140 audit fix: an
// un-shifted NEUTRAL fleet is no longer invisible to the fleet delta cap.
// Two RUNNING NEUTRAL bots ($100 each at 2x) park ½×invest×lev of potential
// adverse inventory each — park 200 total — while a LONG bot contributes its
// full +invest×lev delta. A SHIFTED neutral adds shiftPosition×entryPrice to
// delta but NO park (its live inventory is already counted — stacking the
// park would double-count it), and non-RUNNING bots contribute nothing.
func TestCalculateFleetNetDeltaNeutralPark(t *testing.T) {
	env := newPortfolioTestEnv(t)
	ctx := context.Background()

	env.seedRealBot(t, "PFD-NEUTRAL-1_USDT_PERP", "NEUTRAL", "RUNNING", "{}", "",
		decimal.NewFromInt(100), 2)
	env.seedRealBot(t, "PFD-NEUTRAL-2_USDT_PERP", "NEUTRAL", "RUNNING", "{}", "",
		decimal.NewFromInt(100), 2)
	env.seedRealBot(t, "PFD-LONG-1_USDT_PERP", "LONG", "RUNNING", "{}", "",
		decimal.NewFromInt(100), 2)
	// Shifted NEUTRAL: +10 units at entry 2.0 → +20 signed delta, zero park.
	env.seedRealBot(t, "PFD-SHIFTED-1_USDT_PERP", "NEUTRAL", "RUNNING",
		`{"shiftPosition":"10"}`, `{"entryPrice":"2.0"}`, decimal.NewFromInt(100), 2)
	// Stopped NEUTRAL: excluded by the RUNNING filter, park included.
	env.seedRealBot(t, "PFD-STOPPED-1_USDT_PERP", "NEUTRAL", "STOPPED", "{}", "",
		decimal.NewFromInt(100), 2)

	delta, park, err := env.worker.calculateFleetNetDelta(ctx, env.settings.ID, false)
	if err != nil {
		t.Fatalf("calculateFleetNetDelta: %v", err)
	}
	// delta = +200 (LONG) + 20 (shifted NEUTRAL inventory) = +220
	if !delta.Equal(decimal.NewFromInt(220)) {
		t.Errorf("delta = %s, want 220", delta.String())
	}
	// park = 2 × ½×100×2 = 200 (only the two un-shifted RUNNING NEUTRALs)
	if !park.Equal(decimal.NewFromInt(200)) {
		t.Errorf("neutral park = %s, want 200", park.String())
	}
}

// TestCalculateFleetNetDeltaNeutralParkPaper is the PAPER arm of the same
// query: the park must be visible in the sandbox fleet too — the paper cap
// call site charges it identically against directional candidates.
func TestCalculateFleetNetDeltaNeutralParkPaper(t *testing.T) {
	env := newPortfolioTestEnv(t)
	ctx := context.Background()

	env.seedPaperBot(t, "PFDP-NEUTRAL-1_USDT_PERP", "NEUTRAL", "RUNNING", "{}",
		decimal.NewFromInt(50), 4)
	env.seedPaperBot(t, "PFDP-SHORT-1_USDT_PERP", "SHORT", "RUNNING", "{}",
		decimal.NewFromInt(50), 4)

	delta, park, err := env.worker.calculateFleetNetDelta(ctx, env.settings.ID, true)
	if err != nil {
		t.Fatalf("calculateFleetNetDelta(paper): %v", err)
	}
	if !delta.Equal(decimal.NewFromInt(-200)) {
		t.Errorf("paper delta = %s, want -200", delta.String())
	}
	if !park.Equal(decimal.NewFromInt(100)) {
		t.Errorf("paper neutral park = %s, want 100", park.String())
	}
}

// TestMarginReserveBlocker walks the v2.0.140 reserve line from both sides:
// projected committed isolated margin (Σ active quote_investment + the
// deploy's full slot, tranche doubling included) must leave ≥30% of the last
// recorded equity free — here equity 1000 and 600 already committed, so the
// ceiling is 700.
func TestMarginReserveBlocker(t *testing.T) {
	env := newPortfolioTestEnv(t)
	ctx := context.Background()

	env.seedSnapshot(t, decimal.NewFromInt(1000), time.Now().Add(-time.Minute))
	env.seedRealBot(t, "PFD-COMMITTED-1_USDT_PERP", "NEUTRAL", "RUNNING", "{}", "",
		decimal.NewFromInt(600), 2)

	// Tranche doubling: 600 + 2×200 = 1000 > 700 → blocked.
	code, reason := marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(200), true)
	if code != "MARGIN_RESERVE" {
		t.Errorf("add 200 trancheOn: code = %q, want MARGIN_RESERVE", code)
	}
	if !strings.Contains(reason, "резерв маржи") || !strings.Contains(reason, "projected $1000.00") {
		t.Errorf("add 200 trancheOn: reason = %q, want projected $1000.00 inside", reason)
	}

	// 600 + 2×80 = 760 > 700 → still blocked.
	code, _ = marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(80), true)
	if code != "MARGIN_RESERVE" {
		t.Errorf("add 80 trancheOn: code = %q, want MARGIN_RESERVE", code)
	}

	// Exactly on the line: 600 + 2×50 = 700 is NOT > 700 → allowed (the
	// 30% floor is a floor, not a moat).
	code, _ = marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(50), true)
	if code != "" {
		t.Errorf("add 50 trancheOn: code = %q, want allowed (700 is not > 700)", code)
	}

	// 600 + 2×40 = 680 → allowed.
	code, _ = marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(40), true)
	if code != "" {
		t.Errorf("add 40 trancheOn: code = %q, want allowed", code)
	}

	// Without tranches the slot commits single: 600 + 101 = 701 > 700 →
	// blocked; 600 + 90 = 690 → allowed.
	code, _ = marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(101), false)
	if code != "MARGIN_RESERVE" {
		t.Errorf("add 101 trancheOff: code = %q, want MARGIN_RESERVE", code)
	}
	code, _ = marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(90), false)
	if code != "" {
		t.Errorf("add 90 trancheOff: code = %q, want allowed", code)
	}

	// Fail-open applies ONLY to an account with no snapshot at all: an
	// account id with no rows has no equity truth yet — the snapshot lane
	// bootstraps within 5 minutes of start, and the gate must not deadlock
	// the fleet.
	code, _ = marginReserveBlocker(ctx, env.pool, "00000000-0000-0000-0000-000000000000",
		decimal.NewFromInt(999999), true)
	if code != "" {
		t.Errorf("missing snapshot: code = %q, want fail-open", code)
	}

	// A snapshot that exists is enforced AS-IS, whatever its age: stale
	// equity is the last known truth, not permission to over-commit.
	if _, err := env.pool.Exec(ctx,
		`DELETE FROM account_equity_snapshots WHERE account_id = $1`, env.account.ID); err != nil {
		t.Fatalf("clear snapshots: %v", err)
	}
	env.seedSnapshot(t, decimal.NewFromInt(1000), time.Now().Add(-48*time.Hour))
	code, _ = marginReserveBlocker(ctx, env.pool, env.account.ID,
		decimal.NewFromInt(200), true)
	if code != "MARGIN_RESERVE" {
		t.Errorf("stale snapshot: code = %q, want MARGIN_RESERVE (present ⇒ enforced)", code)
	}
}
