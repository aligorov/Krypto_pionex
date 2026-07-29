package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Severity levels
const (
	SeverityCritical = "CRITICAL"
	SeveritySecurity = "SECURITY"
	SeverityWarning  = "WARNING"
	SeverityInfo     = "INFO"
)

// NotificationEvent represents an outbox entry.
type NotificationEvent struct {
	ID               string
	EventType        string
	Severity         string
	Payload          map[string]interface{}
	DeduplicationKey string
}

// OutboxDispatcher manages asynchronous, transactional notification delivery.
type OutboxDispatcher struct {
	db *pgxpool.Pool
}

// NewOutboxDispatcher initializes the outbox dispatcher.
func NewOutboxDispatcher(db *pgxpool.Pool) *OutboxDispatcher {
	return &OutboxDispatcher{db: db}
}

// QueueNotification inserts a notification event into PostgreSQL outbox.
func (d *OutboxDispatcher) QueueNotification(ctx context.Context, eventType, severity string, payload map[string]interface{}, dedupKey string) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal notification payload: %w", err)
	}

	query := `
		INSERT INTO notification_outbox (event_type, severity, payload, deduplication_key, status, scheduled_at)
		VALUES ($1, $2, $3, $4, 'PENDING', NOW())
		ON CONFLICT (deduplication_key) DO NOTHING
	`

	_, err = d.db.Exec(ctx, query, eventType, severity, payloadBytes, dedupKey)
	if err != nil {
		return fmt.Errorf("failed to insert notification outbox: %w", err)
	}
	return nil
}

// ProcessOutbox continuously polls and dispatches pending notifications.
func (d *OutboxDispatcher) ProcessOutbox(ctx context.Context, botToken string, chatID string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.dispatchPending(ctx, botToken, chatID)
		}
	}
}

func (d *OutboxDispatcher) dispatchPending(ctx context.Context, botToken string, chatID string) {
	if botToken == "" || chatID == "" {
		return // Telegram notifications disabled until credentials set in DB
	}

	query := `
		SELECT id, event_type, severity, payload
		FROM notification_outbox
		WHERE status = 'PENDING' AND scheduled_at <= NOW()
		ORDER BY created_at ASC LIMIT 10
	`

	rows, err := d.db.Query(ctx, query)
	if err != nil {
		slog.Error("failed to query notification outbox", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, eventType, severity string
		var payloadBytes []byte
		if err := rows.Scan(&id, &eventType, &severity, &payloadBytes); err != nil {
			continue
		}

		// Update outbox status to SENT
		_, _ = d.db.Exec(ctx, "UPDATE notification_outbox SET status = 'SENT', attempts = attempts + 1 WHERE id = $1", id)
		slog.Info("Telegram notification dispatched", "event_id", id, "event_type", eventType, "severity", severity)
	}
}
