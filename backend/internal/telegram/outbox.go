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
	db           *pgxpool.Pool
	defaultToken string
	defaultChat  string
	httpClient   *http.Client
	logger       *slog.Logger
	lastUpdateID int64
	service      *Service
}

func NewOutboxDispatcher(db *pgxpool.Pool, defaultToken, defaultChat string) *OutboxDispatcher {
	return &OutboxDispatcher{
		db:           db,
		defaultToken: defaultToken,
		defaultChat:  defaultChat,
		httpClient: &http.Client{
			Timeout: 25 * time.Second,
		},
		logger:  slog.Default(),
		service: NewService(db, slog.Default()),
	}
}

func (d *OutboxDispatcher) getActiveCredentials(ctx context.Context) (token, chatID, topicID string, enabled bool) {
	if settings, err := d.service.GetSettings(ctx); err == nil && settings != nil {
		if settings.Enabled && strings.TrimSpace(settings.BotToken) != "" && strings.TrimSpace(settings.ChatID) != "" {
			return strings.TrimSpace(settings.BotToken), strings.TrimSpace(settings.ChatID), strings.TrimSpace(settings.TopicID), true
		}
	}
	if strings.TrimSpace(d.defaultToken) != "" && strings.TrimSpace(d.defaultChat) != "" {
		return strings.TrimSpace(d.defaultToken), strings.TrimSpace(d.defaultChat), "", true
	}
	return "", "", "", false
}

func (d *OutboxDispatcher) DispatchPending(ctx context.Context) error {
	token, chatID, topicID, enabled := d.getActiveCredentials(ctx)
	if !enabled || token == "" || chatID == "" {
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
		// Extract text if payload is JSON with {"text": "..."}
		msgText := item.payload
		var parsed map[string]any
		if err := json.Unmarshal([]byte(item.payload), &parsed); err == nil {
			if t, ok := parsed["text"].(string); ok && strings.TrimSpace(t) != "" {
				msgText = t
			}
		}

		url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

		var replyMarkup *InlineKeyboardMarkup
		// If payload is a critical alert, candidate, or stop, attach quick action buttons
		if strings.Contains(msgText, "ALERT") || strings.Contains(msgText, "STOP") || strings.Contains(msgText, "Kill Switch") {
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
			"chat_id":    chatID,
			"text":       msgText,
			"parse_mode": "HTML",
		}
		if topicID != "" {
			bodyData["message_thread_id"] = topicID
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
	token, chatID, _, enabled := d.getActiveCredentials(ctx)
	if !enabled || token == "" || chatID == "" {
		return
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=2", token, d.lastUpdateID+1)
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
			if senderID == chatID {
				d.handleCommand(ctx, token, chatID, u.Message.Text)
			}
		}

		if u.CallbackQuery != nil {
			senderID := strconv.FormatInt(u.CallbackQuery.From.ID, 10)
			if senderID == chatID {
				d.handleCallback(ctx, token, chatID, u.CallbackQuery.ID, u.CallbackQuery.Data)
			}
		}
	}
}

func (d *OutboxDispatcher) handleCommand(ctx context.Context, token, chatID, text string) {
	text = strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(text, "/status") || text == "Статус":
		d.sendStatusReport(ctx, token, chatID)
	case strings.HasPrefix(text, "/kill") || text == "🚨 EMERGENCY KILL SWITCH":
		d.triggerKillSwitch(ctx, token, chatID)
	case strings.HasPrefix(text, "/start"):
		d.sendMessage(ctx, token, chatID, "🤖 <b>Pionex Grid Bot Controller</b>\n\nКоманды:\n/status — Текущее состояние ботов и PnL\n/kill — Экстренная остановка (Kill Switch)")
	}
}

func (d *OutboxDispatcher) handleCallback(ctx context.Context, token, chatID, queryID, action string) {
	// Acknowledge callback query
	ackURL := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", token)
	ackBody, _ := json.Marshal(map[string]string{"callback_query_id": queryID})
	_, _ = d.httpClient.Post(ackURL, "application/json", bytes.NewReader(ackBody))

	switch action {
	case "cmd_status":
		d.sendStatusReport(ctx, token, chatID)
	case "cmd_kill_switch":
		d.triggerKillSwitch(ctx, token, chatID)
	}
}

func (d *OutboxDispatcher) sendStatusReport(ctx context.Context, token, chatID string) {
	var paperCount, realCount int
	_ = d.db.QueryRow(ctx, "SELECT COUNT(*) FROM paper_grid_bots WHERE status = 'RUNNING'").Scan(&paperCount)
	_ = d.db.QueryRow(ctx, "SELECT COUNT(*) FROM grid_bots WHERE status = 'RUNNING'").Scan(&realCount)

	var totalPnL float64
	_ = d.db.QueryRow(ctx, "SELECT COALESCE(SUM(realized_pnl_usdt), 0) FROM paper_grid_bots WHERE status IN ('STOPPED', 'COMPLETED')").Scan(&totalPnL)

	msg := fmt.Sprintf(
		"📊 <b>Pionex Bot Status</b>\n\n🟢 Активных симуляций: <b>%d</b>\n🟢 Реальных ботов: <b>%d</b>\n💰 Зафиксированный PnL: <b>%+.4f USDT</b>\n⏱ Время: %s",
		paperCount, realCount, totalPnL, time.Now().Format("15:04:05 MSK"),
	)
	d.sendMessage(ctx, token, chatID, msg)
}

func (d *OutboxDispatcher) triggerKillSwitch(ctx context.Context, token, chatID string) {
	_, err := d.db.Exec(ctx, "UPDATE risk_settings SET kill_switch_enabled = true WHERE id = 1")
	if err != nil {
		d.sendMessage(ctx, token, chatID, fmt.Sprintf("❌ Ошибка включения Kill Switch: %v", err))
		return
	}
	d.sendMessage(ctx, token, chatID, "🚨 <b>KILL SWITCH АКТИВИРОВАН!</b>\nВсе новые запуски ботов заблокированы.")
}

func (d *OutboxDispatcher) sendMessage(ctx context.Context, token, chatID, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	bodyData := map[string]any{
		"chat_id":    chatID,
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
