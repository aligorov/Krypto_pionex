package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type BotExecutionEvent struct {
	ID        string           `json:"id"`
	BotID     string           `json:"botId"`
	BotNumber int              `json:"botNumber"`
	BotSource string           `json:"botSource"`
	Symbol    string           `json:"symbol"`
	EventType string           `json:"eventType"`
	Price     *decimal.Decimal `json:"price"`
	PnLUSDT   *decimal.Decimal `json:"pnlUsdt"`
	Details   map[string]any   `json:"details"`
	CreatedAt time.Time        `json:"createdAt"`
}

// LogBotEvent records a durable lifecycle history event for any bot
func LogBotEvent(
	ctx context.Context,
	db *pgxpool.Pool,
	botID string,
	botNumber int,
	botSource string,
	symbol string,
	eventType string,
	price *decimal.Decimal,
	pnlUSDT *decimal.Decimal,
	details map[string]any,
) error {
	if details == nil {
		details = make(map[string]any)
	}
	detailsJSON, _ := json.Marshal(details)

	_, err := db.Exec(ctx, `
		INSERT INTO bot_execution_events (
			bot_id, bot_number, bot_source, symbol,
			event_type, price, pnl_usdt, details, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, NOW())
	`, botID, botNumber, botSource, symbol, eventType, price, pnlUSDT, string(detailsJSON))
	return err
}

// GetBotExecutionEvents returns the entire chronological history of a bot
func (s *Service) GetBotExecutionEvents(ctx context.Context, botID string) ([]BotExecutionEvent, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, bot_id, bot_number, bot_source, symbol,
		       event_type, price, pnl_usdt, details, created_at
		FROM bot_execution_events
		WHERE bot_id = $1 OR symbol = $1
		ORDER BY created_at DESC
		LIMIT 100
	`, botID)
	if err != nil {
		return nil, fmt.Errorf("list bot execution events: %w", err)
	}
	defer rows.Close()

	items := make([]BotExecutionEvent, 0)
	for rows.Next() {
		var item BotExecutionEvent
		var rawDetails any
		if err := rows.Scan(
			&item.ID, &item.BotID, &item.BotNumber, &item.BotSource, &item.Symbol,
			&item.EventType, &item.Price, &item.PnLUSDT, &rawDetails, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan bot execution event: %w", err)
		}
		if d, ok := rawDetails.(map[string]any); ok {
			item.Details = d
		} else {
			item.Details = make(map[string]any)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// QueueTelegramEvent formats and inserts a notification into notification_outbox
func QueueTelegramEvent(
	ctx context.Context,
	db *pgxpool.Pool,
	eventType string,
	vars map[string]any,
) error {
	var enabled bool
	var botToken, chatID string
	var notifyCreated, notifyTake, notifyStop, notifyAdjust, notifyDigest, notifyEmergency bool
	var tmplCreated, tmplTake, tmplStop, tmplAdjust, tmplDigest string

	row := db.QueryRow(ctx, `
		SELECT enabled, bot_token, chat_id,
		       notify_bot_created, notify_take_profit, notify_stop_loss,
		       notify_range_adjust, notify_digest, notify_emergency,
		       template_bot_created, template_take_profit, template_stop_loss,
		       template_range_adjust, template_digest
		FROM telegram_settings
		WHERE id = 1
	`)
	if err := row.Scan(
		&enabled, &botToken, &chatID,
		&notifyCreated, &notifyTake, &notifyStop,
		&notifyAdjust, &notifyDigest, &notifyEmergency,
		&tmplCreated, &tmplTake, &tmplStop,
		&tmplAdjust, &tmplDigest,
	); err != nil || !enabled || strings.TrimSpace(botToken) == "" || strings.TrimSpace(chatID) == "" {
		return nil // Telegram disabled or not configured
	}

	var tmpl string
	var shouldSend bool

	switch eventType {
	case "BOT_CREATED":
		shouldSend = notifyCreated
		tmpl = tmplCreated
	case "TAKE_PROFIT":
		shouldSend = notifyTake
		tmpl = tmplTake
	case "STOP_LOSS":
		shouldSend = notifyStop
		tmpl = tmplStop
	case "TRANCHE_2":
		tmpl = tmplAdjust // same rendering slot as range shifts; vars overlap
	case "RANGE_ADJUST", "ADJUST_RANGE":
		shouldSend = notifyAdjust
		tmpl = tmplAdjust
	case "DIGEST":
		shouldSend = notifyDigest
		tmpl = tmplDigest
	case "EMERGENCY":
		shouldSend = notifyEmergency
		tmpl = "🚨 <b>EMERGENCY ALERT</b>\n{{message}}"
	default:
		shouldSend = true
		tmpl = "🔔 <b>Уведомление:</b> {{message}}"
	}

	if !shouldSend || strings.TrimSpace(tmpl) == "" {
		return nil
	}

	// Render placeholders
	rendered := tmpl
	for k, v := range vars {
		rendered = strings.ReplaceAll(rendered, fmt.Sprintf("{{%s}}", k), fmt.Sprintf("%v", v))
	}

	payload := map[string]any{
		"text":       rendered,
		"event_type": eventType,
		"created_at": time.Now().Format(time.RFC3339),
	}
	payloadBytes, _ := json.Marshal(payload)

	_, err := db.Exec(ctx, `
		INSERT INTO notification_outbox (event_type, payload, status)
		VALUES ($1, $2::jsonb, 'PENDING')
	`, eventType, string(payloadBytes))
	return err
}
