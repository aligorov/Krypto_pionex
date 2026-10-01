package telegram

import (
	"context"
	"fmt"
	"html"
	"time"
)

func (d *OutboxDispatcher) SetBuildInfo(version, commit, buildTime string) {
	d.version, d.commit, d.buildTime = version, commit, buildTime
}

func (d *OutboxDispatcher) reportHealth(ctx context.Context, token, chatID string) {
	var failed, pending, queued, undelivered int
	var lastError string
	err := d.db.QueryRow(ctx, `SELECT COUNT(*),COALESCE((SELECT LEFT(last_error,200) FROM grid_bots
		WHERE status='FAILED' AND last_error IS NOT NULL ORDER BY created_at DESC LIMIT 1),'')
		FROM grid_bots WHERE status='FAILED' AND created_at>NOW()-INTERVAL '3 hours'`).Scan(&failed, &lastError)
	if err == nil {
		err = d.db.QueryRow(ctx, `SELECT COUNT(*) FROM grid_bots WHERE reconciliation_state='TERMINAL_FINAL_PENDING_EXCHANGE'`).Scan(&pending)
	}
	if err == nil {
		err = d.db.QueryRow(ctx, `SELECT COUNT(*) FILTER(WHERE status IN ('PENDING','SENDING')),
		COUNT(*) FILTER(WHERE status='FAILED' AND created_at>NOW()-INTERVAL '24 hours') FROM notification_outbox`).Scan(&queued, &undelivered)
	}
	var lastSupervision, lastSpot, lastLiq *time.Time
	if err == nil {
		err = d.db.QueryRow(ctx, `SELECT MAX(t.captured_at) FROM bot_telemetry t
		JOIN grid_bots b ON b.id=t.bot_id WHERE b.status IN ('RUNNING','STOP_REQUESTED','STOPPING')`).Scan(&lastSupervision)
	}
	if err == nil {
		err = d.db.QueryRow(ctx, `SELECT MAX(captured_at) FROM account_equity_snapshots WHERE source='bot_spot_aggregate'`).Scan(&lastSpot)
	}
	var connected bool
	if err == nil {
		err = d.db.QueryRow(ctx, `SELECT COALESCE(h.connected,false),h.last_message_at
		FROM (SELECT COALESCE((SELECT value#>>'{}' FROM app_config WHERE key='liquidation_source'),'bybit') AS source) configured
		LEFT JOIN liquidation_feed_health h USING(source)`).Scan(&connected, &lastLiq)
	}
	var lastPoll *time.Time
	var pollError *string
	if err == nil {
		err = d.db.QueryRow(ctx, `SELECT MAX(last_poll_at),MAX(last_error) FROM telegram_poll_state WHERE token_fingerprint=$1`, tokenFingerprint(token)).Scan(&lastPoll, &pollError)
	}
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ здоровье: данные недоступны — "+escapeErr(err))
		return
	}
	text := fmt.Sprintf("❤️ <b>ЗДОРОВЬЕ СИСТЕМЫ</b>\n\nВерсия: %s · commit %s\nСборка: %s\n"+
		"Провалы создания за 3ч: %d\nЖдут биржевого финала: %d\n"+
		"Доставка: %d в очереди, %d ошибок за 24ч\nНадзор — последняя телеметрия: %s\nSpot-снимок: %s\n"+
		"Ликвидации: соединение %t · heartbeat %s\nTelegram — последний успешный опрос: %s\nПоследняя ошибка создания: <code>%s</code>",
		html.EscapeString(d.version), html.EscapeString(d.commit), html.EscapeString(d.buildTime), failed, pending, queued, undelivered,
		freshness(lastSupervision, 2*time.Minute), freshness(lastSpot, 10*time.Minute), connected, freshness(lastLiq, 15*time.Minute), freshness(lastPoll, time.Minute), html.EscapeString(lastError))
	if pollError != nil {
		text += "\nОшибка опроса: <code>" + html.EscapeString(*pollError) + "</code>"
	}
	d.sendMessage(ctx, token, chatID, text)
}

func freshness(t *time.Time, maxAge time.Duration) string {
	if t == nil {
		return "нет данных"
	}
	state := "✅"
	age := time.Since(*t)
	if age < 0 || age > maxAge {
		state = "⚠️"
	}
	return state + " " + t.UTC().Format(time.RFC3339)
}
