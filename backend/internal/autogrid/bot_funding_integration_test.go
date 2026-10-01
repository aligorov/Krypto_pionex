package autogrid

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"testing"
	"time"
)

// seedBotFundingSnapshot gives an integration env a fresh Spot-funding
// snapshot so the fail-closed capital gates pass and the suite can exercise
// whatever OTHER gate it targets. available is the spendable Spot USDT;
// invested seeds assets_usdt (already-inside-bots capital, not spendable).
func seedBotFundingSnapshot(t *testing.T, pool *pgxpool.Pool, accountID string, available, invested float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO account_equity_snapshots
			(account_id, equity_usdt, assets_usdt, available_usdt, unrealized_pnl_usdt, source, captured_at)
		VALUES ($1, $2, $3, $4, 0, $5, NOW())
	`, accountID, available+invested, invested, available, equitySnapshotSourceBotAggregate)
	if err != nil {
		t.Fatalf("seed bot funding snapshot: %v", err)
	}
}

func TestBotFundingSourceAndReservations(t *testing.T) {
	env := newPortfolioTestEnv(t)
	ctx := context.Background()
	_, err := env.pool.Exec(ctx, `INSERT INTO account_equity_snapshots(account_id,equity_usdt,source) VALUES ($1,9999,'bot_aggregate')`, env.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, err := env.worker.capitalEffectiveBudget(ctx, env.account.ID, decimal.NewFromInt(100), true, decimal.NewFromInt(30)); !v.IsZero() || err == nil {
		t.Fatal("legacy Futures wallet must not authorize Spot spending")
	}
	env.seedSnapshot(t, decimal.NewFromInt(253), time.Now())
	botID := env.seedRealBot(t, "PFD-FUNDING_USDT_PERP", "NEUTRAL", "RUNNING", `{"trancheDeployed":1,"trancheBase":"100"}`, "", decimal.NewFromInt(50), 2)
	if v, _, err := env.worker.capitalEffectiveBudget(ctx, env.account.ID, decimal.NewFromInt(100), true, decimal.NewFromInt(30)); !v.Equal(decimal.NewFromInt(77)) || err != nil {
		t.Fatalf("unpaid tranche not reserved: %s", v)
	}
	if code, reason := marginReserveBlocker(ctx, env.pool, env.account.ID, decimal.NewFromInt(50), false, decimal.NewFromInt(30), botID); code != "" {
		t.Fatalf("reserved top-up charged twice: %s", reason)
	}
	_, err = env.pool.Exec(ctx, `UPDATE account_equity_snapshots SET available_usdt=0 WHERE account_id=$1 AND source='bot_spot_aggregate'`, env.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, err := env.worker.capitalEffectiveBudget(ctx, env.account.ID, decimal.NewFromInt(100), true, decimal.NewFromInt(30)); !v.IsZero() || err != nil {
		t.Fatalf("locked equity is not spendable: %s", v)
	}
}

func TestBotFundingSpotCapture(t *testing.T) {
	env := newEquityTestEnv(t)
	env.mock.mu.Lock()
	env.mock.walletUSDT = "253"
	env.mock.mu.Unlock()
	env.worker.captureBotAggregateEquity(context.Background(), *env.settings)
	var equity, available decimal.Decimal
	if err := env.pool.QueryRow(context.Background(), `SELECT equity_usdt,available_usdt FROM account_equity_snapshots WHERE account_id=$1 AND source='bot_spot_aggregate' ORDER BY captured_at DESC LIMIT 1`, env.account.ID).Scan(&equity, &available); err != nil {
		t.Fatal(err)
	}
	if !equity.Equal(decimal.NewFromInt(253)) || !available.Equal(equity) {
		t.Fatalf("Spot funding missing: equity=%s available=%s", equity, available)
	}
}
