package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Settings lock makes the queue row and cadence checkpoint one transaction.
func (d *OutboxDispatcher) QueueDigestIfDue(ctx context.Context) error {
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var template string
	err = tx.QueryRow(ctx, `SELECT template_digest FROM telegram_settings WHERE id=1 AND enabled AND notify_digest
		AND bot_token<>'' AND chat_id<>'' AND digest_interval_minutes BETWEEN 1 AND 1440
		AND (last_digest_at IS NULL OR last_digest_at<=NOW()-(digest_interval_minutes*INTERVAL '1 minute'))
		FOR UPDATE SKIP LOCKED`).Scan(&template)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(template) == "" {
		return nil
	}
	var bots int
	var realized, floating decimal.Decimal
	err = tx.QueryRow(ctx, `SELECT COUNT(*),COALESCE(SUM(COALESCE(realized_pnl_usdt,0)-COALESCE(fees_paid_usdt,0)),0),
		COALESCE(SUM(unrealized_pnl_usdt),0) FROM grid_bots WHERE execution_mode='REAL'
		AND status IN ('RUNNING','STOP_REQUESTED','STOPPING') AND bu_order_id IS NOT NULL`).Scan(&bots, &realized, &floating)
	if err != nil {
		return err
	}
	var balance string
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT CASE WHEN captured_at BETWEEN NOW()-INTERVAL '10 minutes' AND NOW()
		THEN available_usdt::text ELSE 'снимок устарел' END FROM account_equity_snapshots
		WHERE source='bot_spot_aggregate' ORDER BY captured_at DESC LIMIT 1),'нет свежего снимка')`).Scan(&balance)
	if err != nil {
		return err
	}
	text := RenderTemplate(template, map[string]any{"active_bots": bots, "total_pnl": realized.Add(floating).StringFixed(4), "realized_pnl": realized.StringFixed(4), "balance_usdt": balance})
	if strings.Contains(text, "{{") {
		text = "📊 <b>Сводка</b>\nАктивных ботов: " + decimal.NewFromInt(int64(bots)).String() + "\nНетто открытых: " + realized.Add(floating).StringFixed(4) + " USDT\nСвободный Spot USDT: " + balance
	}
	raw, err := json.Marshal(map[string]any{"text": text, "event_type": "DIGEST", "created_at": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO notification_outbox(event_type,payload) VALUES ('DIGEST',$1::jsonb)`, string(raw)); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_settings SET last_digest_at=NOW() WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
