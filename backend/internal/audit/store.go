package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	Action       string         `json:"action"`
	Actor        string         `json:"actor"`
	ActorID      *string        `json:"actorId"`
	ActorType    string         `json:"actorType"`
	ResourceType string         `json:"resourceType"`
	ResourceID   string         `json:"resourceId"`
	Outcome      string         `json:"outcome"`
	RequestID    string         `json:"requestId"`
	IPAddress    string         `json:"ipAddress"`
	UserAgent    string         `json:"userAgent"`
	Details      map[string]any `json:"details"`
	CreatedAt    time.Time      `json:"createdAt"`
}

type Filter struct {
	Action       string
	ActorID      string
	ResourceType string
	Outcome      string
	Limit        int
}

type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

func EventFromPrincipal(
	principal auth.Principal,
	action, resourceType, resourceID, outcome string,
	details map[string]any,
) Event {
	actorID := principal.UserID
	return Event{
		Action:       action,
		Actor:        principal.Username,
		ActorID:      &actorID,
		ActorType:    principal.ActorType,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Outcome:      outcome,
		Details:      details,
	}
}

func (s *Store) Record(ctx context.Context, event Event) error {
	if event.Actor == "" {
		event.Actor = "system"
	}
	if event.ActorType == "" {
		event.ActorType = "SYSTEM"
	}
	if event.Outcome == "" {
		event.Outcome = "SUCCESS"
	}
	if event.Details == nil {
		event.Details = map[string]any{}
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO audit_events (
			action, actor, actor_id, actor_type, resource_type, resource_id,
			outcome, request_id, ip_address, user_agent, details
		) VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''),
		          $7, NULLIF($8, ''), NULLIF($9, '')::INET, NULLIF($10, ''), $11)
	`, event.Action, event.Actor, event.ActorID, event.ActorType, event.ResourceType,
		event.ResourceID, event.Outcome, event.RequestID, event.IPAddress,
		event.UserAgent, event.Details)
	if err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context, filter Filter) ([]Event, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT action, actor, actor_id, actor_type, COALESCE(resource_type, ''),
		       COALESCE(resource_id, ''), outcome, COALESCE(request_id, ''),
		       COALESCE(host(ip_address), ''), COALESCE(user_agent, ''),
		       details, created_at
		FROM audit_events
		WHERE ($1 = '' OR action = $1)
		  AND ($2 = '' OR actor_id::TEXT = $2)
		  AND ($3 = '' OR resource_type = $3)
		  AND ($4 = '' OR outcome = $4)
		ORDER BY created_at DESC
		LIMIT $5
	`, filter.Action, filter.ActorID, filter.ResourceType, filter.Outcome, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		if err := rows.Scan(
			&event.Action, &event.Actor, &event.ActorID, &event.ActorType,
			&event.ResourceType, &event.ResourceID, &event.Outcome,
			&event.RequestID, &event.IPAddress, &event.UserAgent,
			&event.Details, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
