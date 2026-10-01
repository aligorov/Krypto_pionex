package autogrid

import (
	"context"
	"strings"
	"testing"
)

func TestDeployRealFundingDataFailureIsNotCapitalDeficiency(t *testing.T) {
	for _, tc := range []struct {
		name, update, code, outcome string
		capitalDeficiency           bool
	}{
		{"stale", "UPDATE account_equity_snapshots SET captured_at=NOW()-INTERVAL '11 minutes' WHERE account_id=$1", "FUNDING_STALE", "WAIT", false},
		{"future", "UPDATE account_equity_snapshots SET captured_at=NOW()+INTERVAL '11 minutes' WHERE account_id=$1", "FUNDING_STALE", "WAIT", false},
		{"missing", "DELETE FROM account_equity_snapshots WHERE account_id=$1", "FUNDING_UNAVAILABLE", "WAIT", false},
		{"insufficient", "UPDATE account_equity_snapshots SET equity_usdt=1, available_usdt=1, assets_usdt=0 WHERE account_id=$1", "MARGIN_RESERVE", "REJECT", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const symbol = "FUNDINGCASE_USDT_PERP"
			h := newRealDeployHarness(t, 6, symbol)
			h.cleanupSymbol(t, symbol)
			ctx := context.Background()
			scanID := h.seedAcceptedCandidate(t, symbol)
			if _, err := h.pool.Exec(ctx, tc.update, h.account.ID); err != nil {
				t.Fatal(err)
			}
			if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
				t.Fatal(err)
			}
			var code, outcome string
			if err := h.pool.QueryRow(ctx, `SELECT code, outcome FROM entry_decisions
				WHERE symbol=$1 ORDER BY created_at DESC LIMIT 1`, symbol).Scan(&code, &outcome); err != nil {
				t.Fatal(err)
			}
			if code != tc.code || outcome != tc.outcome {
				t.Fatalf("want %s/%s, got %s/%s", tc.outcome, tc.code, outcome, code)
			}
			if !h.worker.lastCapitalAlarmAt.IsZero() != tc.capitalDeficiency {
				t.Fatalf("capital deficiency alarm must only reflect insufficient funds")
			}
			decision, reason := h.candidateRow(t, scanID, symbol)
			if decision != "REJECTED" || reason == "" {
				t.Fatalf("candidate must show the reason for deferral: %s %q", decision, reason)
			}
			if !tc.capitalDeficiency && strings.Contains(reason, "резерв маржи") {
				t.Fatalf("funding-data failure must not display a capital shortfall: %q", reason)
			}
			var bots int
			if err := h.pool.QueryRow(ctx, `SELECT COUNT(*) FROM grid_bots WHERE account_id=$1`, h.account.ID).Scan(&bots); err != nil {
				t.Fatal(err)
			}
			if bots != 0 || h.mock.createCalls.Load() != 0 {
				t.Fatal("deferred funding must not create a local bot or call Pionex create")
			}
		})
	}
}
