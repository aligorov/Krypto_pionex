package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func parseTopic(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid Telegram topic id")
	}
	return n, nil
}

type replyContextKey struct{}
type replyContext struct {
	key     string
	part    int
	topicID string
	failed  bool
}

func (d *OutboxDispatcher) queueReply(ctx context.Context, chatID, text string, kb InlineKeyboardMarkup) error {
	settings, err := d.service.GetSettings(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{"text": text, "chat_id": chatID, "topic_id": settings.TopicID, "reply_markup": kb}
	if reply, ok := ctx.Value(replyContextKey{}).(*replyContext); ok && reply.topicID != "" {
		payload["topic_id"] = reply.topicID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var dedup any
	if reply, ok := ctx.Value(replyContextKey{}).(*replyContext); ok {
		dedup = fmt.Sprintf("tg:%s:%d", reply.key, reply.part)
		reply.part++
	}
	_, err = d.db.Exec(ctx, `INSERT INTO notification_outbox(event_type,payload,deduplication_key)
		VALUES ('TG_CONSOLE',$1::jsonb,$2) ON CONFLICT(deduplication_key) DO NOTHING`, string(raw), dedup)
	return err
}

type notification struct {
	Text     string                `json:"text"`
	Message  string                `json:"message"`
	ChatID   string                `json:"chat_id"`
	TopicID  string                `json:"topic_id"`
	Keyboard *InlineKeyboardMarkup `json:"reply_markup"`
}

func decodeNotification(raw, event string) (notification, error) {
	var n notification
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		// Legacy producers may have written a JSON string rather than an object.
		if json.Unmarshal([]byte(raw), &n.Text) != nil {
			return n, fmt.Errorf("invalid notification payload")
		}
	}
	if n.Text == "" {
		n.Text = n.Message
	}
	if strings.TrimSpace(n.Text) == "" {
		return n, fmt.Errorf("notification has no text or message")
	}
	if n.Keyboard == nil && (strings.HasPrefix(event, "EMERGENCY") || strings.Contains(event, "STOP") || event == "WS_RATE_LIMIT") {
		n.Keyboard = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "📊 Статус", CallbackData: "cmd_status"}, {Text: "🚨 KILL SWITCH", CallbackData: "cmd_kill_switch"}}}}
	}
	return n, nil
}

// A claim is atomic, ordered and leased. Competing dispatchers never send
// the same row concurrently; a crash releases it after two minutes.
func (d *OutboxDispatcher) DispatchPending(ctx context.Context) error {
	token, chat, topic, enabled := d.getActiveCredentials(ctx)
	if !enabled {
		return nil
	}
	var paused bool
	if err := d.db.QueryRow(ctx, `SELECT COALESCE((SELECT send_paused_until>NOW() FROM telegram_poll_state WHERE token_fingerprint=$1),false)`, tokenFingerprint(token)).Scan(&paused); err != nil {
		return err
	}
	if paused {
		return nil
	}
	var id, raw, event string
	var part, failures int
	err := d.db.QueryRow(ctx, `WITH next AS (
		SELECT id FROM notification_outbox WHERE
		(status='PENDING' OR status='SENDING') AND scheduled_at<=NOW()
		ORDER BY CASE WHEN event_type='TG_CONSOLE' THEN 0 WHEN severity='CRITICAL' THEN 1 ELSE 2 END,created_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
		UPDATE notification_outbox n SET status='SENDING',scheduled_at=NOW()+INTERVAL '2 minutes',attempts=attempts+1
		FROM next WHERE n.id=next.id RETURNING n.id,n.payload::text,n.event_type,n.sent_parts,n.consecutive_failures`).Scan(&id, &raw, &event, &part, &failures)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	n, err := decodeNotification(raw, event)
	var messageID int64
	var parts []string
	if err == nil {
		if n.ChatID != "" {
			chat = n.ChatID
			topic = n.TopicID
		}
		parts = splitHTML(n.Text)
		if part < 0 || part >= len(parts) {
			err = fmt.Errorf("invalid notification part cursor")
		} else {
			var kb *InlineKeyboardMarkup
			if part == len(parts)-1 {
				kb = n.Keyboard
			}
			messageID, err = d.sendPart(ctx, token, chat, topic, parts[part], kb)
		}
	}
	if err == nil {
		state := "PENDING"
		if part+1 == len(parts) {
			state = "SENT"
		}
		_, err = d.db.Exec(ctx, `UPDATE notification_outbox SET status=$2::varchar,sent_parts=sent_parts+1,
			consecutive_failures=0,last_error=NULL,telegram_message_id=$3,scheduled_at=NOW(),
			sent_at=CASE WHEN $2::varchar='SENT' THEN NOW() ELSE NULL END WHERE id=$1`, id, state, messageID)
		return err
	}
	failures++
	delay := time.Duration(1<<min(failures, 8)) * time.Second
	state := "PENDING"
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.RetryAfter > 0 {
			delay = time.Duration(apiErr.RetryAfter) * time.Second
		}
		if apiErr.Code == 429 {
			_, pauseErr := d.db.Exec(ctx, `INSERT INTO telegram_poll_state(token_fingerprint,send_paused_until)
			VALUES ($1,NOW()+($2::double precision*INTERVAL '1 second')) ON CONFLICT(token_fingerprint)
			DO UPDATE SET send_paused_until=GREATEST(telegram_poll_state.send_paused_until,EXCLUDED.send_paused_until)`, tokenFingerprint(token), delay.Seconds())
			if pauseErr != nil {
				return pauseErr
			}
		}
		if apiErr.Code == 400 || apiErr.Code == 403 {
			state = "FAILED"
		}
		// Token replacement may repair 401 later; leave a bounded retry trail.
	}
	if failures >= 8 {
		state = "FAILED"
	}
	d.logger.Warn("telegram delivery failed", "component", "telegram_outbox", "notification_id", id, "event", event, "state", state, "error", err.Error())
	_, saveErr := d.db.Exec(ctx, `UPDATE notification_outbox SET status=$2,last_error=$3,
		consecutive_failures=$4,scheduled_at=NOW()+($5::double precision*INTERVAL '1 second') WHERE id=$1`, id, state, err.Error(), failures, delay.Seconds())
	return saveErr
}
