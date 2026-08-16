package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Settings struct {
	ID                    int        `json:"id"`
	Enabled               bool       `json:"enabled"`
	BotToken              string     `json:"botToken"`
	ChatID                string     `json:"chatID"`
	TopicID               string     `json:"topicID"`
	NotifyBotCreated      bool       `json:"notifyBotCreated"`
	NotifyTakeProfit      bool       `json:"notifyTakeProfit"`
	NotifyStopLoss        bool       `json:"notifyStopLoss"`
	NotifyRangeAdjust     bool       `json:"notifyRangeAdjust"`
	NotifyDigest          bool       `json:"notifyDigest"`
	NotifyEmergency       bool       `json:"notifyEmergency"`
	DigestIntervalMinutes int        `json:"digestIntervalMinutes"`
	TemplateBotCreated    string     `json:"templateBotCreated"`
	TemplateTakeProfit    string     `json:"templateTakeProfit"`
	TemplateStopLoss      string     `json:"templateStopLoss"`
	TemplateRangeAdjust   string     `json:"templateRangeAdjust"`
	TemplateDigest        string     `json:"templateDigest"`
	LastDigestAt          *time.Time `json:"lastDigestAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type Service struct {
	db         *pgxpool.Pool
	httpClient *http.Client
	logger     *slog.Logger
}

func NewService(db *pgxpool.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db: db,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		logger: logger,
	}
}

func (s *Service) GetSettings(ctx context.Context) (*Settings, error) {
	row := s.db.QueryRow(ctx, `
		SELECT id, enabled, bot_token, chat_id, topic_id,
		       notify_bot_created, notify_take_profit, notify_stop_loss,
		       notify_range_adjust, notify_digest, notify_emergency,
		       digest_interval_minutes, template_bot_created, template_take_profit,
		       template_stop_loss, template_range_adjust, template_digest,
		       last_digest_at, updated_at
		FROM telegram_settings
		WHERE id = 1
	`)
	var item Settings
	if err := row.Scan(
		&item.ID, &item.Enabled, &item.BotToken, &item.ChatID, &item.TopicID,
		&item.NotifyBotCreated, &item.NotifyTakeProfit, &item.NotifyStopLoss,
		&item.NotifyRangeAdjust, &item.NotifyDigest, &item.NotifyEmergency,
		&item.DigestIntervalMinutes, &item.TemplateBotCreated, &item.TemplateTakeProfit,
		&item.TemplateStopLoss, &item.TemplateRangeAdjust, &item.TemplateDigest,
		&item.LastDigestAt, &item.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("get telegram settings: %w", err)
	}
	return &item, nil
}

func (s *Service) UpdateSettings(ctx context.Context, item Settings) (*Settings, error) {
	// The API never returns the raw token (audit SEC-003), so an unchanged
	// token arrives empty or masked: keep the stored value in that case.
	current, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(item.BotToken)
	if token == "" || strings.Contains(token, "•") {
		token = current.BotToken
	}
	_, err = s.db.Exec(ctx, `
		UPDATE telegram_settings
		SET enabled = $2,
		    bot_token = $3,
		    chat_id = $4,
		    topic_id = $5,
		    notify_bot_created = $6,
		    notify_take_profit = $7,
		    notify_stop_loss = $8,
		    notify_range_adjust = $9,
		    notify_digest = $10,
		    notify_emergency = $11,
		    digest_interval_minutes = $12,
		    template_bot_created = $13,
		    template_take_profit = $14,
		    template_stop_loss = $15,
		    template_range_adjust = $16,
		    template_digest = $17,
		    updated_at = NOW()
		WHERE id = 1
	`,
		item.ID, item.Enabled, token, item.ChatID, item.TopicID,
		item.NotifyBotCreated, item.NotifyTakeProfit, item.NotifyStopLoss,
		item.NotifyRangeAdjust, item.NotifyDigest, item.NotifyEmergency,
		item.DigestIntervalMinutes, item.TemplateBotCreated, item.TemplateTakeProfit,
		item.TemplateStopLoss, item.TemplateRangeAdjust, item.TemplateDigest,
	)
	if err != nil {
		return nil, fmt.Errorf("update telegram settings: %w", err)
	}
	updated, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	updated.BotToken = MaskToken(updated.BotToken)
	return updated, nil
}

// MaskToken renders a token safe for API responses: first and last 4
// characters only. The raw token must never leave the server (SEC-003).
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 8 {
		if token == "" {
			return ""
		}
		return "••••••••"
	}
	return token[:4] + "••••••••" + token[len(token)-4:]
}

func (s *Service) TestConnection(ctx context.Context, token, chatID, topicID string) error {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" || chatID == "" {
		return errors.New("bot token and chat id cannot be empty")
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	body := map[string]any{
		"chat_id":    chatID,
		"text":       "✅ <b>Pionex Trading Bot: Тестовое сообщение</b>\n\nСвязь с Telegram успешно установлена! Уведомления настроены и готовы к работе.",
		"parse_mode": "HTML",
	}
	if strings.TrimSpace(topicID) != "" {
		body["message_thread_id"] = topicID
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var tgErr struct {
			Description string `json:"description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&tgErr)
		return fmt.Errorf("telegram api error (%d): %s", resp.StatusCode, tgErr.Description)
	}
	return nil
}

// RenderTemplate replaces {{key}} placeholders in template string
func RenderTemplate(tmpl string, vars map[string]any) string {
	result := tmpl
	for k, v := range vars {
		placeholder := fmt.Sprintf("{{%s}}", k)
		result = strings.ReplaceAll(result, placeholder, fmt.Sprintf("%v", v))
	}
	return result
}

// EnqueueNotification inserts a formatted message into notification_outbox
func (s *Service) EnqueueNotification(ctx context.Context, eventType string, vars map[string]any) error {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled || settings.BotToken == "" || settings.ChatID == "" {
		return nil // notifications disabled
	}

	var tmpl string
	var allowed bool

	switch eventType {
	case "BOT_CREATED":
		allowed = settings.NotifyBotCreated
		tmpl = settings.TemplateBotCreated
	case "TAKE_PROFIT":
		allowed = settings.NotifyTakeProfit
		tmpl = settings.TemplateTakeProfit
	case "STOP_LOSS":
		allowed = settings.NotifyStopLoss
		tmpl = settings.TemplateStopLoss
	case "RANGE_ADJUST":
		allowed = settings.NotifyRangeAdjust
		tmpl = settings.TemplateRangeAdjust
	case "DIGEST":
		allowed = settings.NotifyDigest
		tmpl = settings.TemplateDigest
	case "EMERGENCY":
		allowed = settings.NotifyEmergency
		tmpl = "🚨 <b>EMERGENCY ALERT</b>\n{{message}}"
	default:
		allowed = true
		tmpl = "🔔 <b>Уведомление:</b> {{message}}"
	}

	if !allowed || strings.TrimSpace(tmpl) == "" {
		return nil
	}

	rendered := RenderTemplate(tmpl, vars)
	payload := map[string]any{
		"text":       rendered,
		"event_type": eventType,
		"created_at": time.Now().Format(time.RFC3339),
	}
	payloadBytes, _ := json.Marshal(payload)

	_, err = s.db.Exec(ctx, `
		INSERT INTO notification_outbox (event_type, payload, status)
		VALUES ($1, $2::jsonb, 'PENDING')
	`, eventType, string(payloadBytes))
	return err
}
