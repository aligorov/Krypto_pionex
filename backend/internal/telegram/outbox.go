package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type OutboxDispatcher struct {
	db         *pgxpool.Pool
	botToken   string
	chatID     string
	httpClient *http.Client
}

func NewOutboxDispatcher(db *pgxpool.Pool, botToken, chatID string) *OutboxDispatcher {
	return &OutboxDispatcher{
		db:       db,
		botToken: botToken,
		chatID:   chatID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (d *OutboxDispatcher) DispatchPending(ctx context.Context) error {
	if d.botToken == "" || d.chatID == "" {
		return nil // Dispatcher is unconfigured
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
		bodyData := map[string]string{
			"chat_id": d.chatID,
			"text":    item.payload,
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
