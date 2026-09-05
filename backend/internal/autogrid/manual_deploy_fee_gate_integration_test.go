package autogrid

import (
	"context"
	"strings"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// TestManualDeployFeeGateParity pins the v2.0.89-A P1 fee-gate on the manual
// deploy path: «level step ≥ 2× round-trip costs» on the FINAL geometry
// (lower/upper over the row AFTER every fallback), evaluated BEFORE the
// mode branch so PAPER and REAL behave identically — same rule, same
// numbers, same text. Fleet fees pinned at 5/2 bps: round trip 0.14%,
// minimum step 0.28%.
func TestManualDeployFeeGateParity(t *testing.T) {
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
	snapshotManualSettings(t, pool, settings.ID)

	// Pin the friction assumptions the gate reads (snapshot + restore only
	// the columns this test mutates beyond the shared snapshot).
	var savedFee, savedSlip string
	if err := pool.QueryRow(ctx, `
		SELECT fee_bps::TEXT, slippage_bps::TEXT FROM autogrid_settings WHERE id = $1
	`, settings.ID).Scan(&savedFee, &savedSlip); err != nil {
		t.Fatalf("snapshot fee settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE autogrid_settings
		SET budget_usdt = 500, fee_bps = 5, slippage_bps = 2,
		    pnl_target_mode = 'STATIC', pnl_target_usdt = 6, max_loss_usdt = 8
		WHERE id = $1
	`, settings.ID); err != nil {
		t.Fatalf("pin settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `
			UPDATE autogrid_settings
			SET fee_bps = $2::NUMERIC, slippage_bps = $3::NUMERIC
			WHERE id = $1
		`, settings.ID, savedFee, savedSlip); err != nil {
			t.Errorf("restore fee settings: %v", err)
		}
	})
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'CANFG%_USDT_PERP'`); err != nil {
			t.Errorf("cleanup paper bots: %v", err)
		}
	})

	// Public exchange mock: the PAPER pass case needs the live PERP price
	// (100) and klines for the DYNAMIC/STATIC target math. The reject cases
	// never reach the network — the gate fires first.
	mock := newManualDeployExchangeMock(t, "30")
	service.publicAPI = pionex.NewClient(mock.server.URL, "", "")

	deploy := func(symbol, mode string, lower, upper float64, row int) error {
		t.Helper()
		input := ManualDeployInput{
			Symbol: symbol, Mode: mode, Direction: "NEUTRAL", Leverage: 2,
			Lower: decimal.NewFromFloat(lower), Upper: decimal.NewFromFloat(upper), Row: row,
		}
		_, _, deployErr := service.DeployManualBot(ctx, nil, input)
		return deployErr
	}

	// 4% span over 20 levels → 0.20% step < 0.28%: refused in REAL…
	if err := deploy("CANFG1_USDT_PERP", "REAL", 98, 102, 20); err == nil ||
		!strings.Contains(err.Error(), "fee-gate") ||
		!strings.Contains(err.Error(), "0.20%") ||
		!strings.Contains(err.Error(), "0.14%") {
		t.Fatalf("REAL 4%%/20-levels deploy must be refused by the fee-gate naming both numbers, got %v", err)
	}
	// …and in PAPER with the identical text — parity before the mode branch.
	if err := deploy("CANFG2_USDT_PERP", "PAPER", 98, 102, 20); err == nil ||
		!strings.Contains(err.Error(), "fee-gate") ||
		!strings.Contains(err.Error(), "0.20%") {
		t.Fatalf("PAPER 4%%/20-levels deploy must be refused by the same fee-gate, got %v", err)
	}

	// Derived row on a 2% span: GridLevelsForRange(2%, $1000 notional) →
	// 8 levels × 0.25% — the density floor lands UNDER the 2× round-trip
	// bar, so the manual deploy is refused too.
	if err := deploy("CANFG3_USDT_PERP", "PAPER", 99, 101, 0); err == nil ||
		!strings.Contains(err.Error(), "fee-gate") {
		t.Fatalf("derived-row 2%%-span deploy must be refused by the fee-gate, got %v", err)
	}

	// Nothing was written for any refused deploy.
	var refusedRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots WHERE symbol LIKE 'CANFG%_USDT_PERP'
	`).Scan(&refusedRows); err != nil || refusedRows != 0 {
		t.Fatalf("refused deploys must leave no paper rows, got %d (%v)", refusedRows, err)
	}

	// Pass side: 4%/12 levels = 0.33% ≥ 0.28% — the PAPER deploy goes
	// through and persists the operator's row untouched.
	if err := deploy("CANFG4_USDT_PERP", "PAPER", 98, 102, 12); err != nil {
		t.Fatalf("4%%/12-levels deploy must clear the fee-gate, got %v", err)
	}
	var persistedRow int
	if err := pool.QueryRow(ctx, `
		SELECT grid_num FROM paper_grid_bots WHERE symbol = 'CANFG4_USDT_PERP' AND status = 'RUNNING'
	`).Scan(&persistedRow); err != nil || persistedRow != 12 {
		t.Fatalf("fee-gate-passing deploy must persist row 12, got %d (%v)", persistedRow, err)
	}

	// Boundary documentation (5/2 bps): an explicit 6-row grid on a 2% span
	// steps 0.33% — ABOVE the 0.28% bar — and is legal; the rejecting case
	// on a 2% span is the density-derived 8 rows above.
	if err := deploy("CANFG5_USDT_PERP", "PAPER", 99, 101, 6); err != nil {
		t.Fatalf("2%%/6-levels (0.33%% step) must clear the 0.28%% bar at 5/2 bps, got %v", err)
	}
}
