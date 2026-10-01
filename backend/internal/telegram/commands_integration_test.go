package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
)

func TestMenuCommandsUseConfirmedNetResults(t *testing.T) {
	dispatcher := telegramTestDispatcher(t)
	ctx := context.Background()
	account, err := accounts.NewService(dispatcher.db).Create(ctx, accounts.CreateInput{Name: "telegram-audit", APIKey: "test-key", APISecret: "test-secret", HasBotPermission: true, HasFuturesPermission: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dispatcher.db.Exec(ctx, `DELETE FROM grid_bots WHERE account_id=$1`, account.ID)
		dispatcher.db.Exec(ctx, `DELETE FROM pionex_accounts WHERE id=$1`, account.ID)
	})
	for _, tc := range []struct {
		status, state string
		final, fee    string
	}{
		{"RUNNING", "REST_AUTHORITATIVE_OK", "3", "0.4"},
		{"STOPPED", "REMOTE_TERMINAL_CONFIRMED", "10", "1"},
		{"STOPPED", "TERMINAL_FINAL_PENDING_EXCHANGE", "123", "0"},
	} {
		_, err := dispatcher.db.Exec(ctx, `INSERT INTO grid_bots(account_id,symbol,status,direction,grid_type,
			lower_price,upper_price,grid_num,leverage,quote_investment,request_fingerprint,bu_order_id,
			realized_pnl_usdt,unrealized_pnl_usdt,fees_paid_usdt,reconciliation_state,closed_at,model_state,execution_mode)
			VALUES ($1,'TGTEST_USDT_PERP',$2::varchar,'NEUTRAL','ARITHMETIC',1,2,5,2,50,md5(random()::text),md5(random()::text),
			$3,-0.2,$4,$5,CASE WHEN $2::varchar='STOPPED' THEN NOW() ELSE NULL END,'{"closeCostUsdt":"0.3"}'::jsonb,'REAL')`, account.ID, tc.status, tc.final, tc.fee, tc.state)
		if err != nil {
			t.Fatal(err)
		}
	}
	fleet, err := loadFleet(ctx, dispatcher.db)
	if err != nil || len(fleet) != 1 || fleet[0].Realized.String() != "2.6" {
		t.Fatalf("running fees lost: %+v %v", fleet, err)
	}
	closed, err := loadClosed(ctx, dispatcher.db, 10, 0)
	if err != nil || len(closed) != 1 || closed[0].Final.String() != "9.3" {
		t.Fatalf("unconfirmed/fees mixed: %+v %v", closed, err)
	}
	for _, cmd := range []string{"/start", "/help", "/status", "/bots", "/closed 50", "/day", "/stats", "/pnl", "/risk", "/health", "МЕНЮ", "/STATUS@test_bot"} {
		dispatcher.routeCommand(ctx, "test-token", "42", cmd)
		var payload string
		if err := dispatcher.db.QueryRow(ctx, `SELECT payload::text FROM notification_outbox ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var n notification
		if err := json.Unmarshal([]byte(payload), &n); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(n.Text, "❌") || strings.TrimSpace(n.Text) == "" {
			t.Fatalf("%s failed: %s", cmd, n.Text)
		}
		if n.TopicID != "7" {
			t.Fatalf("%s missed topic", cmd)
		}
	}
	// Every visible menu action routes to a response and acknowledges its callback.
	for _, row := range mainMenuKeyboard().InlineKeyboard {
		for _, button := range row {
			dispatcher.handleCallback(ctx, "test-token", "42", "callback-test", button.CallbackData)
		}
	}
	var health string
	dispatcher.reportHealth(ctx, "test-token", "42")
	if err := dispatcher.db.QueryRow(ctx, `SELECT payload->>'text' FROM notification_outbox ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&health); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(health, "test-version") || strings.Contains(health, "надзор: активен") {
		t.Fatal("health still invents liveness")
	}
}

func TestKillSwitchHonorsPermissionAndAudits(t *testing.T) {
	dispatcher := telegramTestDispatcher(t)
	ctx := context.Background()
	dispatcher.triggerKillSwitch(ctx, "test-token", "42")
	var kill bool
	if err := dispatcher.db.QueryRow(ctx, `SELECT kill_switch_enabled FROM risk_settings WHERE id=1`).Scan(&kill); err != nil {
		t.Fatal(err)
	}
	if kill {
		t.Fatal("disabled write permission was bypassed")
	}
	if _, err := dispatcher.db.Exec(ctx, `UPDATE app_config SET value='true'::jsonb WHERE key='telegram_write_commands_enabled'`); err != nil {
		t.Fatal(err)
	}
	dispatcher.triggerKillSwitch(ctx, "test-token", "42")
	if err := dispatcher.db.QueryRow(ctx, `SELECT kill_switch_enabled FROM risk_settings WHERE id=1`).Scan(&kill); err != nil || !kill {
		t.Fatalf("authorized kill failed: %v %v", kill, err)
	}
	var audited bool
	if err := dispatcher.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE actor='telegram:42' AND action='risk.kill_switch.enable')`).Scan(&audited); err != nil || !audited {
		t.Fatalf("no audit: %v", err)
	}
}
