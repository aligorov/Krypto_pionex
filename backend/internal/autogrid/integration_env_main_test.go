package autogrid

// v2.0.184: the integration suite runs WITHOUT a quant-worker, so the
// migration-default backtest_gate=true turns every deploy-path test into
// 3×75s of waitForBacktest timeouts (and flips nondeterministically between
// runs as tests toggle the flag). One authoritative default for the whole
// environment: gate OFF at suite start. Tests that pin the gate itself
// enable it in their own setup (see TestPaperDeployRunsBacktestGate).

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	if dbURL := os.Getenv("PIONEX_TEST_DATABASE_URL"); dbURL != "" {
		if pool, err := pgxpool.New(context.Background(), dbURL); err == nil {
			_, _ = pool.Exec(context.Background(),
				`UPDATE feature_flags SET enabled = false, updated_at = NOW() WHERE name = 'backtest_gate'`)
			pool.Close()
		}
	}
	os.Exit(m.Run())
}
