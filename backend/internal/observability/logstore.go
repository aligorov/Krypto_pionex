package observability

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type LogEntry struct {
	ID          int64          `json:"id"`
	OccurredAt  time.Time      `json:"occurredAt"`
	Level       string         `json:"level"`
	Component   string         `json:"component"`
	Message     string         `json:"message"`
	RequestID   string         `json:"requestId"`
	ActorID     *string        `json:"actorId"`
	AccountID   *string        `json:"accountId"`
	Symbol      string         `json:"symbol"`
	AggregateID string         `json:"aggregateId"`
	Fields      map[string]any `json:"fields"`
}

type LogFilter struct {
	Level     string
	Component string
	RequestID string
	Search    string
	Limit     int
}

type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

func (s *Store) Write(ctx context.Context, entry LogEntry) error {
	if entry.Level == "" {
		entry.Level = "INFO"
	}
	if entry.Component == "" {
		entry.Component = "application"
	}
	if entry.Fields == nil {
		entry.Fields = map[string]any{}
	}
	entry.Fields = RedactFields(entry.Fields)
	_, err := s.db.Exec(ctx, `
		INSERT INTO application_logs (
			level, component, message, request_id, actor_id, account_id,
			symbol, aggregate_id, fields
		) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, NULLIF($7, ''),
		          NULLIF($8, ''), $9)
	`, entry.Level, entry.Component, entry.Message, entry.RequestID, entry.ActorID,
		entry.AccountID, entry.Symbol, entry.AggregateID, entry.Fields)
	if err != nil {
		return fmt.Errorf("write application log: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context, filter LogFilter) ([]LogEntry, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, occurred_at, level, component, message,
		       COALESCE(request_id, ''), actor_id, account_id,
		       COALESCE(symbol, ''), COALESCE(aggregate_id, ''), fields
		FROM application_logs
		WHERE ($1 = '' OR level = upper($1))
		  AND ($2 = '' OR component = $2)
		  AND ($3 = '' OR request_id = $3)
		  AND ($4 = '' OR message ILIKE '%' || $4 || '%')
		ORDER BY occurred_at DESC
		LIMIT $5
	`, filter.Level, filter.Component, filter.RequestID, filter.Search, limit)
	if err != nil {
		return nil, fmt.Errorf("list application logs: %w", err)
	}
	defer rows.Close()

	entries := make([]LogEntry, 0)
	for rows.Next() {
		var entry LogEntry
		if err := rows.Scan(
			&entry.ID, &entry.OccurredAt, &entry.Level, &entry.Component,
			&entry.Message, &entry.RequestID, &entry.ActorID, &entry.AccountID,
			&entry.Symbol, &entry.AggregateID, &entry.Fields,
		); err != nil {
			return nil, fmt.Errorf("scan application log: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

type handlerCore struct {
	store   *Store
	queue   chan LogEntry
	dropped atomic.Uint64
}

type DBHandler struct {
	base   slog.Handler
	core   *handlerCore
	attrs  []slog.Attr
	groups []string
}

func NewDBHandler(base slog.Handler, store *Store) *DBHandler {
	core := &handlerCore{
		store: store,
		queue: make(chan LogEntry, 2048),
	}
	go core.run()
	return &DBHandler{base: base, core: core}
}

func (h *DBHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *DBHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := h.base.Handle(ctx, record); err != nil {
		return err
	}
	fields := make(map[string]any)
	for _, attr := range h.attrs {
		addAttr(fields, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		addAttr(fields, h.groups, attr)
		return true
	})
	entry := LogEntry{
		OccurredAt:  record.Time,
		Level:       levelName(record.Level),
		Component:   stringValue(fields, "component", "application"),
		Message:     record.Message,
		RequestID:   stringValue(fields, "request_id", ""),
		Symbol:      stringValue(fields, "symbol", ""),
		AggregateID: stringValue(fields, "aggregate_id", ""),
		Fields:      RedactFields(fields),
	}
	select {
	case h.core.queue <- entry:
	default:
		h.core.dropped.Add(1)
	}
	return nil
}

func (h *DBHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := &DBHandler{
		base:   h.base.WithAttrs(attrs),
		core:   h.core,
		attrs:  append(append([]slog.Attr{}, h.attrs...), attrs...),
		groups: append([]string{}, h.groups...),
	}
	return cloned
}

func (h *DBHandler) WithGroup(name string) slog.Handler {
	cloned := &DBHandler{
		base:   h.base.WithGroup(name),
		core:   h.core,
		attrs:  append([]slog.Attr{}, h.attrs...),
		groups: append(append([]string{}, h.groups...), name),
	}
	return cloned
}

func (h *DBHandler) Dropped() uint64 {
	return h.core.dropped.Load()
}

func (c *handlerCore) run() {
	for entry := range c.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.store.Write(ctx, entry)
		cancel()
	}
}

func RedactFields(fields map[string]any) map[string]any {
	redacted := make(map[string]any, len(fields))
	for key, value := range fields {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "password") ||
			strings.Contains(lower, "secret") ||
			strings.Contains(lower, "signature") ||
			strings.Contains(lower, "token") ||
			strings.Contains(lower, "api_key") ||
			strings.Contains(lower, "authorization") ||
			strings.Contains(lower, "database_url") {
			redacted[key] = "[REDACTED]"
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			redacted[key] = RedactFields(nested)
			continue
		}
		redacted[key] = value
	}
	return redacted
}

func addAttr(target map[string]any, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := attr.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	target[key] = attr.Value.Any()
}

func stringValue(fields map[string]any, key, fallback string) string {
	value, ok := fields[key]
	if !ok {
		return fallback
	}
	text, ok := value.(string)
	if !ok {
		return fallback
	}
	return text
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level <= slog.LevelDebug:
		return "DEBUG"
	default:
		return "INFO"
	}
}
