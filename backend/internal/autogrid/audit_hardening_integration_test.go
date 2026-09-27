package autogrid

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func TestLiquidationHealthUsesTransport(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	w := &Worker{db: pool}
	// All changes are confined to the disposable integration database.
	var oldSource string
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT value::text FROM app_config WHERE key='liquidation_source'), 'null')`).Scan(&oldSource); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO app_config (key,value) VALUES ('liquidation_source','"audit-test"') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM liquidation_feed_health WHERE source='audit-test'`)
		if oldSource == "null" {
			_, _ = pool.Exec(ctx, `DELETE FROM app_config WHERE key='liquidation_source'`)
		} else {
			_, _ = pool.Exec(ctx, `UPDATE app_config SET value=$1::jsonb WHERE key='liquidation_source'`, oldSource)
		}
	}()
	if healthy, _ := w.LiquidationSourceHealthy(ctx); healthy {
		t.Fatal("missing transport must block")
	}
	for _, tc := range []struct {
		connected bool
		age       string
		want      bool
	}{
		{true, "0 seconds", true}, // Healthy even without liquidation events.
		{false, "0 seconds", false},
		{true, "16 minutes", false},
		{true, "-1 hour", false},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO liquidation_feed_health(source,connected,last_message_at)
            VALUES ('audit-test',$1,NOW()-$2::interval) ON CONFLICT(source) DO UPDATE
            SET connected=EXCLUDED.connected,last_message_at=EXCLUDED.last_message_at`, tc.connected, tc.age)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := w.LiquidationSourceHealthy(ctx); got != tc.want {
			t.Fatalf("%+v: healthy=%v", tc, got)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if healthy, _ := w.LiquidationSourceHealthy(cancelled); healthy {
		t.Fatal("SQL failure must block")
	}
}

func TestCloseIntentAndFinalSurviveRestart(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	account, err := accounts.NewService(pool).Create(ctx, accounts.CreateInput{
		Name: "audit-close-intent", APIKey: "itest-key", APISecret: "itest-secret", HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id=$1`, account.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id=$1`, account.ID)
	}()
	var id string
	err = pool.QueryRow(ctx, `INSERT INTO grid_bots(account_id,symbol,status,direction,grid_type,
        lower_price,upper_price,grid_num,leverage,quote_investment,request_fingerprint)
        VALUES ($1,'AUDIT_USDT_PERP','RUNNING','NEUTRAL','ARITHMETIC',1,2,4,2,100,md5(random()::text)) RETURNING id`, account.ID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, total := range []int64{-8, -12} {
		if active, err := w.recordCloseIntent(ctx, id, "STOP_LOSS", decimal.NewFromInt(total)); err != nil || !active {
			t.Fatalf("intent: %v %v", active, err)
		}
	}
	var marker string
	if err := pool.QueryRow(ctx, `SELECT model_state->>'stopIntentTotal' FROM grid_bots WHERE id=$1`, id).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "-8" {
		t.Fatalf("first intent overwritten: %s", marker)
	}
	_, err = pool.Exec(ctx, `UPDATE grid_bots SET status='STOPPED',closed_at=NOW(),reconciliation_state=$2 WHERE id=$1`, id, TerminalFinalPendingExchange)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := w.recordCloseIntent(ctx, id, "STOP_LOSS", decimal.NewFromInt(-20)); err != nil || active {
		t.Fatalf("terminal mutated: %v %v", active, err)
	}
	w.applyExchangeFinal(ctx, pendingTerminalFinal{id: id, symbol: "AUDIT_USDT_PERP"}, decimal.NewFromInt(-9), pionex.FinalProfitUnlockIdentity)
	w.healV103UnlockIdentity(ctx)
	w.terminalIdentityHealDone = false
	w.healV103UnlockIdentity(ctx)
	var final, state string
	if err := pool.QueryRow(ctx, `SELECT realized_pnl_usdt::text,reconciliation_state FROM grid_bots WHERE id=$1`, id).Scan(&final, &state); err != nil {
		t.Fatal(err)
	}
	if !decimal.RequireFromString(final).Equal(decimal.NewFromInt(-9)) || state != "REMOTE_TERMINAL_CONFIRMED" {
		t.Fatalf("restart damaged final: %s %s", final, state)
	}
	// Legacy rows with no calculation provenance must also be preserved.
	_, err = pool.Exec(ctx, `UPDATE grid_bots SET model_state=model_state-'unlockIdentityBasis' WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	w.terminalIdentityHealDone = false
	w.healV103UnlockIdentity(ctx)
	if err := pool.QueryRow(ctx, `SELECT reconciliation_state FROM grid_bots WHERE id=$1`, id).Scan(&state); err != nil || state != "REMOTE_TERMINAL_CONFIRMED" {
		t.Fatalf("unversioned final reopened: %s %v", state, err)
	}
}
