package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type OutboxDispatcher struct {
	db          *pgxpool.Pool
	botToken    string
	chatID      string
	httpClient  *http.Client
	logger      *slog.Logger
	lastUpdateID int64
}

func NewOutboxDispatcher(db *pgxpool.Pool, botToken, chatID string) *OutboxDispatcher {
	return &OutboxDispatcher{
		db:       db,
		botToken: botToken,
		chatID:   chatID,
		httpClient: &http.Client{
			Timeout: 25 * time.Second,
		},
		logger: slog.Default(),
	}
}

func (d *OutboxDispatcher) DispatchPending(ctx context.Context) error {
	if d.botToken == "" || d.chatID == "" {
		return nil
	}

	rows, err := d.db.Query(ctx, "SELECT id, payload #>> '{}' FROM notification_outbox WHERE status = 'PENDING' LIMIT 10")
	if err != nil {
		return err
	}
	defer rows.Close()

	type msgItem struct {
		id      string
		payload string
	}
	var items []msgItem
	for rows.Next() {
		var item msgItem
		if err := rows.Scan(&item.id, &item.payload); err == nil {
			items = append(items, item)
		}
	}

	for _, item := range items {
		url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
		
		var replyMarkup *InlineKeyboardMarkup
		// If payload is a critical alert, attach action buttons
		if strings.Contains(item.payload, "ALERT") || strings.Contains(item.payload, "CANDIDATE") || strings.Contains(item.payload, "STOP") {
			replyMarkup = &InlineKeyboardMarkup{
				InlineKeyboard: [][]InlineKeyboardButton{
					{
						{Text: "📊 Статус ботов", CallbackData: "cmd_status"},
						{Text: "🚨 KILL SWITCH", CallbackData: "cmd_kill_switch"},
					},
				},
			}
		}

		bodyData := map[string]any{
			"chat_id":    d.chatID,
			"text":       item.payload,
			"parse_mode": "HTML",
		}
		if replyMarkup != nil {
			bodyData["reply_markup"] = replyMarkup
		}

		jsonBytes, _ := json.Marshal(bodyData)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := d.httpClient.Do(req)
		if err != nil {
			_, _ = d.db.Exec(ctx, "UPDATE notification_outbox SET attempts = attempts + 1, status = 'FAILED' WHERE id = $1", item.id)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			_, _ = d.db.Exec(ctx, "UPDATE notification_outbox SET status = 'SENT', attempts = attempts + 1 WHERE id = $1", item.id)
		} else {
			_, _ = d.db.Exec(ctx, "UPDATE notification_outbox SET attempts = attempts + 1, status = 'FAILED' WHERE id = $1", item.id)
		}
	}

	return nil
}

// StartInboundListener starts the long-polling loop for 2-way Telegram commands
func (d *OutboxDispatcher) StartInboundListener(ctx context.Context) {
	if d.botToken == "" || d.chatID == "" {
		return
	}

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
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=2", d.botToken, d.lastUpdateID+1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var data struct {
		Ok     bool `json:"ok"`
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  *struct {
				MessageID int64 `json:"message_id"`
				From      struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				Text string `json:"text"`
			} `json:"message"`
			CallbackQuery *struct {
				ID   string `json:"id"`
				From struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Data string `json:"data"`
			} `json:"callback_query"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || !data.Ok {
		return
	}

	for _, u := range data.Result {
		if u.UpdateID > d.lastUpdateID {
			d.lastUpdateID = u.UpdateID
		}

		// Security: verify authorized chat ID
		if u.Message != nil {
			senderID := strconv.FormatInt(u.Message.Chat.ID, 10)
			if senderID == d.chatID {
				d.handleCommand(ctx, u.Message.Text)
			}
		}

		if u.CallbackQuery != nil {
			senderID := strconv.FormatInt(u.CallbackQuery.From.ID, 10)
			if senderID == d.chatID {
				d.handleCallback(ctx, u.CallbackQuery.ID, u.CallbackQuery.Data)
			}
		}
	}
}

func (d *OutboxDispatcher) handleCommand(ctx context.Context, text string) {
	text = strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(text, "/status") || text == "Статус":
		d.sendStatusReport(ctx)
	case strings.HasPrefix(text, "/kill") || text == "🚨 EMERGENCY KILL SWITCH":
		d.triggerKillSwitch(ctx)
	case strings.HasPrefix(text, "/start"):
		d.sendMessage(ctx, "🤖 <b>Pionex Grid Bot Controller</b>\n\nКоманды:\n/status — Текущее состояние ботов и PnL\n/kill — Экстренная остановка (Kill Switch)")
	}
}

func (d *OutboxDispatcher) handleCallback(ctx context.Context, queryID, action string) {
	// Acknowledge callback query
	ackURL := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", d.botToken)
	ackBody, _ := json.Marshal(map[string]string{"callback_query_id": queryID})
	_, _ = d.httpClient.Post(ackURL, "application/json", bytes.NewReader(ackBody))

	switch action {
	case "cmd_status":
		d.sendStatusReport(ctx)
	case "cmd_kill_switch":
		d.triggerKillSwitch(ctx)
	}
}

func (d *OutboxDispatcher) sendStatusReport(ctx context.Context) {
	var count int
	_ = d.db.QueryRow(ctx, "SELECT COUNT(*) FROM grid_bots WHERE status = 'RUNNING'").Scan(&count)

	var totalPnL float64
	_ = d.db.QueryRow(ctx, "SELECT COALESCE(SUM(realized_pnl_usdt), 0) FROM grid_bots WHERE status = 'STOPPED'").Scan(&totalPnL)

	msg := fmt.Sprintf(
		"📊 <b>Pionex Bot Status</b>\n\n🟢 Активных ботов: <b>%d</b>\n💰 Зафиксированный PnL: <b>%.4f USDT</b>\n⏱ Время: %s",
		count, totalPnL, time.Now().Format("15:04:05 MSK"),
	)
	d.sendMessage(ctx, msg)
}

func (d *OutboxDispatcher) triggerKillSwitch(ctx context.Context) {
	_, err := d.db.Exec(ctx, "UPDATE risk_settings SET kill_switch_enabled = true WHERE id = 1")
	if err != nil {
		d.sendMessage(ctx, fmt.Sprintf("❌ Ошибка включения Kill Switch: %v", err))
		return
	}
	d.sendMessage(ctx, "🚨 <b>KILL SWITCH АКТИВИРОВАН!</b>\nВсе новые запуски ботов заблокированы.")
}

func (d *OutboxDispatcher) sendMessage(ctx context.Context, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
	bodyData := map[string]any{
		"chat_id":    d.chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	jsonBytes, _ := json.Marshal(bodyData)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
