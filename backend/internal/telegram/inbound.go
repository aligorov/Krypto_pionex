package telegram

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type telegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}
type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type telegramMessage struct {
	From     telegramUser `json:"from"`
	Chat     telegramChat `json:"chat"`
	Text     string       `json:"text"`
	ThreadID int64        `json:"message_thread_id"`
}
type telegramUpdate struct {
	ID       int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
	Callback *struct {
		ID      string           `json:"id"`
		From    telegramUser     `json:"from"`
		Message *telegramMessage `json:"message"`
		Data    string           `json:"data"`
	} `json:"callback_query"`
}

func tokenFingerprint(token string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(token))) }
func authorizedPrivate(chat telegramChat, from telegramUser, pinned string) bool {
	return chat.Type == "private" && chat.ID > 0 && from.ID == chat.ID && !from.IsBot && strconv.FormatInt(chat.ID, 10) == pinned
}
func authorizedTopic(thread int64, pinned string) bool {
	return pinned == "" || strconv.FormatInt(thread, 10) == pinned
}

func (d *OutboxDispatcher) StartInboundListener(ctx context.Context) {
	instance, _ := os.Hostname()
	d.logger.Info("tg console: inbound listener starting (getUpdates long-poll)", "component", "telegram_console", "instance", instance, "pid", os.Getpid())
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pollUpdates(ctx)
		}
	}
}

func (d *OutboxDispatcher) pollUpdates(ctx context.Context) {
	token, chat, topic, enabled := d.getActiveCredentials(ctx)
	if !enabled {
		if time.Since(d.lastCredsWarnAt) > time.Hour {
			d.lastCredsWarnAt = time.Now()
			d.logger.Warn("tg console: disabled or unconfigured", "component", "telegram_console")
		}
		return
	}
	key := tokenFingerprint(token)
	if key != d.webhookToken {
		d.webhookToken = key
		d.webhookDropped = false
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		d.pollFailure(ctx, key, err)
		return
	}
	defer tx.Rollback(ctx)
	// Shared DB instances serialize polling and read a shared durable cursor.
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "telegram-poll:"+key).Scan(&locked); err != nil || !locked {
		return
	}
	if _, err = tx.Exec(ctx, `INSERT INTO telegram_poll_state(token_fingerprint) VALUES ($1) ON CONFLICT DO NOTHING`, key); err != nil {
		return
	}
	var cursor int64
	var registered bool
	if err = tx.QueryRow(ctx, `SELECT last_update_id,commands_registered FROM telegram_poll_state WHERE token_fingerprint=$1`, key).Scan(&cursor, &registered); err != nil {
		return
	}
	// setMyCommands is a Telegram API write and is rate-limited separately
	// from getUpdates. If another instance or a recent restart hit the limit,
	// persist the server-provided pause and do not immediately retry every
	// three seconds. This also prevents a registration storm from starving
	// the actual command poller.
	var paused bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE(send_paused_until>NOW(),false) FROM telegram_poll_state WHERE token_fingerprint=$1`, key).Scan(&paused); err != nil {
		return
	}
	if paused {
		return
	}
	if !registered {
		if err = callTelegram(ctx, d.httpClient, token, "setMyCommands", map[string]any{"commands": commandCatalog()}, nil); err == nil {
			_, err = tx.Exec(ctx, `UPDATE telegram_poll_state SET commands_registered=true WHERE token_fingerprint=$1`, key)
		}
		if err != nil {
			d.logger.Warn("tg console: command menu registration failed", "component", "telegram_console", "error", err.Error())
			pause := 30 * time.Second
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Code == 429 && apiErr.RetryAfter > 0 {
				pause = time.Duration(apiErr.RetryAfter) * time.Second
			}
			if _, pauseErr := tx.Exec(ctx, `UPDATE telegram_poll_state
				SET send_paused_until=NOW()+($2::double precision*INTERVAL '1 second'),last_error=$3
				WHERE token_fingerprint=$1`, key, pause.Seconds(), err.Error()); pauseErr != nil {
				d.pollFailure(ctx, key, pauseErr)
				return
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				d.pollFailure(ctx, key, commitErr)
			}
			return
		}
	}
	var updates []telegramUpdate
	err = callTelegram(ctx, d.httpClient, token, "getUpdates", map[string]any{"offset": cursor + 1, "timeout": 2, "allowed_updates": []string{"message", "callback_query"}}, &updates)
	if err != nil {
		tx.Rollback(ctx)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 409 && strings.Contains(strings.ToLower(apiErr.Description), "webhook") && !d.webhookDropped {
			if dropErr := d.dropWebhook(ctx, token); dropErr == nil {
				d.webhookDropped = true
			} else {
				d.pollFailure(ctx, key, dropErr)
			}
		}
		d.pollFailure(ctx, key, err)
		return
	}
	for _, u := range updates {
		if u.ID <= cursor {
			continue
		}
		reply := &replyContext{key: fmt.Sprintf("%s:%d", key, u.ID)}
		commandCtx := context.WithValue(ctx, replyContextKey{}, reply)
		if m := u.Message; m != nil && authorizedPrivate(m.Chat, m.From, chat) && authorizedTopic(m.ThreadID, topic) {
			if m.ThreadID > 0 {
				reply.topicID = strconv.FormatInt(m.ThreadID, 10)
			}
			if strings.TrimSpace(m.Text) != "" {
				d.handleCommand(commandCtx, token, chat, m.Text)
			}
		}
		if cb := u.Callback; cb != nil && cb.Message != nil && authorizedPrivate(cb.Message.Chat, cb.From, chat) && authorizedTopic(cb.Message.ThreadID, topic) {
			if cb.Message.ThreadID > 0 {
				reply.topicID = strconv.FormatInt(cb.Message.ThreadID, 10)
			}
			d.handleCallback(commandCtx, token, chat, cb.ID, cb.Data)
		} else if u.Callback != nil {
			d.acknowledgeCallback(ctx, token, u.Callback.ID, "Команды доступны только закреплённому приватному пользователю в настроенной теме.")
		}
		if reply.failed {
			return
		} // Retain cursor: a transient SQL failure must not lose a reply.
		cursor = u.ID
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_poll_state SET last_update_id=$2,last_poll_at=NOW(),last_error=NULL WHERE token_fingerprint=$1`, key, cursor); err != nil {
		return
	}
	if err = tx.Commit(ctx); err != nil {
		d.pollFailure(ctx, key, err)
	}
}

func (d *OutboxDispatcher) pollFailure(ctx context.Context, key string, err error) {
	_, _ = d.db.Exec(ctx, `INSERT INTO telegram_poll_state(token_fingerprint,last_error) VALUES ($1,$2)
 ON CONFLICT(token_fingerprint) DO UPDATE SET last_error=EXCLUDED.last_error`, key, err.Error())
	if time.Since(d.lastPollWarnAt) > 10*time.Minute {
		d.lastPollWarnAt = time.Now()
		d.logger.Warn("tg console: getUpdates failed", "component", "telegram_console", "error", err.Error())
	}
}

func commandCatalog() []map[string]string {
	names := []string{"start", "status", "bots", "closed", "day", "stats", "pnl", "risk", "health", "kill"}
	descriptions := []string{"Меню", "Статус флота", "Активные боты", "Последние закрытия", "День UTC", "Статистика 30 дней", "Результаты 30 дней", "Риск-лимиты", "Состояние системы", "Блокировать новые входы (если разрешено)"}
	out := make([]map[string]string, len(names))
	for i, name := range names {
		out[i] = map[string]string{"command": name, "description": descriptions[i]}
	}
	return out
}
