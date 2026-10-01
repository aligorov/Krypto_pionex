package autogrid

import (
	"log/slog"

	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/telegram"
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

// telegramChannels carries the per-channel switch positions and the
// operator-editable templates read from telegram_settings — the exact inputs
// the per-event routing switch decides on.
type telegramChannels struct {
	notifyCreated, notifyTake, notifyStop, notifyAdjust, notifyDigest, notifyEmergency bool
	tmplCreated, tmplTake, tmplStop, tmplAdjust, tmplDigest                            string
}

// telegramEventRouting decides, per event type, whether the Telegram lane
// fires and which template renders it. Pure on purpose (v2.0.143 audit-2):
// channel semantics are pin-able in a unit test without a database.
func telegramEventRouting(eventType string, ch telegramChannels) (bool, string) {
	return telegram.EventRouting(eventType, telegram.Settings{
		NotifyBotCreated: ch.notifyCreated, NotifyTakeProfit: ch.notifyTake, NotifyStopLoss: ch.notifyStop,
		NotifyRangeAdjust: ch.notifyAdjust, NotifyDigest: ch.notifyDigest, NotifyEmergency: ch.notifyEmergency,
		TemplateBotCreated: ch.tmplCreated, TemplateTakeProfit: ch.tmplTake, TemplateStopLoss: ch.tmplStop,
		TemplateRangeAdjust: ch.tmplAdjust, TemplateDigest: ch.tmplDigest,
	})
}

// QueueTelegramEvent formats and inserts a notification into notification_outbox
func QueueTelegramEvent(ctx context.Context, db *pgxpool.Pool, eventType string, vars map[string]any) error {
	return telegram.NewService(db, nil).EnqueueNotification(ctx, eventType, vars)
}

// recordCandidateOutcome back-fills the deployed candidate's row with the
// bot's final result (v2.0.54): entry features and outcomes then live in
// one table, and score calibration no longer depends on a fragile
// candidate_id JOIN through bot rows. First outcome wins (outcome_at IS
// NULL guard) — a re-entered symbol writes its own new candidate row.
func recordCandidateOutcome(ctx context.Context, db *pgxpool.Pool, candidateID *string, total decimal.Decimal, reason string) {
	if candidateID == nil || strings.TrimSpace(*candidateID) == "" {
		return
	}
	if _, err := db.Exec(ctx, `
		UPDATE autogrid_candidates
		SET outcome_pnl_usdt = $2, outcome_closed_reason = $3, outcome_at = NOW()
		WHERE id = $1 AND outcome_at IS NULL
	`, *candidateID, total, reason); err != nil {
		_ = err // paper-path best-effort as before; the REAL path logs via recordRealBotOutcome
	}
}

// recordRealBotOutcome is the REAL-fleet settle hook (v2.0.165): every
// terminal path that finalizes a native grid bot calls this with the bot id
// and the exchange-truth total — the candidate link rides the grid_bots row
// (migration 0059), so no call site needs to track the candidate itself.
// First outcome wins, mirroring recordCandidateOutcome.
func recordRealBotOutcome(ctx context.Context, db *pgxpool.Pool, logger *slog.Logger, botID string, total decimal.Decimal, reason string) {
	if strings.TrimSpace(botID) == "" || db == nil {
		return
	}
	// v2.0.169 (§3-tail): the confirmed exchange final UPDATES the stored
	// estimate instead of being blocked by the first-outcome-wins guard —
	// the old guard left stale telemetry estimates in the training data
	// forever when the exchange truth landed later. The previous value is
	// preserved in the audit history (outcome_revision + outcome_prev).
	if _, err := db.Exec(ctx, `
		UPDATE autogrid_candidates c
		SET outcome_prev_pnl_usdt = c.outcome_pnl_usdt,
		    outcome_pnl_usdt = $2,
		    outcome_closed_reason = $3,
		    outcome_at = NOW(),
		    outcome_revision = COALESCE(c.outcome_revision, 0) + 1
		FROM grid_bots g
		WHERE g.id = $1 AND g.candidate_id = c.id
		  AND (
		    c.outcome_at IS NULL
		    OR (
		      -- Update only when the figure actually changed (prevents
		      -- same-value churn from repeated settle passes).
		      COALESCE(c.outcome_pnl_usdt, 0) <> $2
		      AND g.reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED'
		    )
		  )
	`, botID, total, reason); err != nil && logger != nil {
		logger.Error("recordRealBotOutcome failed",
			"component", "autogrid_worker", "bot_id", botID, "error", err)
	}
}

// entryFeaturesJSON snapshots the candidate's full feature set into the
// bot row at deploy (v2.0.54): analytics then survive candidate turnover,
// and the entry-vs-outcome dataset no longer depends on a JOIN.
func entryFeaturesJSON(candidate Candidate) []byte {
	if len(candidate.ModelAssumptions) == 0 {
		return []byte(`{}`)
	}
	raw, err := json.Marshal(candidate.ModelAssumptions)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

// fundingDeltaSigned returns the accrual as a signed PAID amount: positive
// when the bot pays funding, negative when it receives.
func fundingDeltaSigned(pays bool, delta decimal.Decimal) decimal.Decimal {
	if pays {
		return delta
	}
	return delta.Neg()
}
