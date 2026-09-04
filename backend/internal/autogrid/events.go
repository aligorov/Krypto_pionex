package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	case "DELIST_SWEEP", "TRANCHE_2":
		// v2.0.19: shouldSend was never set in this branch (var defaults to
		// false), so sweep closes and tranche-2 top-ups were silently
		// dropped from Telegram — operators learned nothing while bots
		// closed "by themselves".
		shouldSend = notifyAdjust
		tmpl = tmplAdjust // same rendering slot as range shifts; vars overlap
	case "TRANCHE_2_SKIPPED":
		// v2.0.56 F2: risk-gated top-up skip — operator must see WHY a bot
		// stays on its first tranche (cap $12 / fleet envelope).
		shouldSend = notifyAdjust
		tmpl = "⛔ <b>Транш-2 отложен:</b> бот #{{bot_number}} {{symbol}} — {{reason}}"
	case "RADAR_RECENTER_FAILED":
		// v2.0.75: the exchange refusing the radar's escape adjust used to be
		// a Warn-only swallow — the operator saw a silent radar while bots sat
		// in band 4 for hours. One line per hour, first-class signal.
		shouldSend = notifyAdjust
		tmpl = "⚠️ <b>Радар: эскейп отклонён биржей:</b> бот #{{bot_number}} {{symbol}} band {{band}} — {{error}}"
	case "RADAR_B2_EARLY_RECENTER":
		// v2.0.76 "shift on green": the preventive band-2 re-center fired
		// while the profit preflight still passes — the classic dwell-3
		// window (v2.0.85: under water the shift now ships keepInvestment
		// instead of being blocked; see RADAR_B2_VELOCITY_RECENTER for the
		// one-tick lane).
		shouldSend = notifyAdjust
		tmpl = "🛡 <b>Радар: ранний ре-центр на зелёном (B2):</b> бот #{{bot_number}} {{symbol}} — цена прошла {{edge_progress_pct}}% пути к опасному краю, score {{score}}, total {{total}} USDT → [{{lower_price}}, {{upper_price}}]"
	case "RADAR_B2_VELOCITY_RECENTER":
		// v2.0.85 "shift early": the trajectory lane — from 55% of the way
		// to the adverse edge, a price racing at ≥ 0.6×ATR(15м)/15м fires
		// after ONE tick (dwell 1); the dwell-3 early window slips past on
		// exactly these pairs. Requires a still-green base (normal shift).
		shouldSend = notifyAdjust
		tmpl = "⚡ <b>Радар: скоростной ре-центр (B2 velocity):</b> бот #{{bot_number}} {{symbol}} — цена на {{edge_progress_pct}}% пути к краю, скорость {{speed_atr_15m}}×ATR/15м, score {{score}}, total {{total}} USDT → [{{lower_price}}, {{upper_price}}]"
	case "RADAR_AUTOCLOSE":
		// v2.0.84: the radar closed the bot itself (opt-in mode BAND3/STRICT).
		// Queued BEFORE the close intent — the operator must see the why
		// (band, score, total at the moment) even when the native stop wins
		// the race.
		shouldSend = notifyStop
		tmpl = "🛑 <b>Радар: автозакрытие ({{mode}}):</b> бот #{{bot_number}} {{symbol}} — band {{band}} (score {{score}}), total {{total}} USDT — {{reason}}"
	case "STOP_FORECAST_SHADOW":
		// v2.0.57: the radar's band transitions used to fall into the
		// generic {{message}} fallback and shipped literal placeholders
		// (prod 2026-09-01: XLM band 2/3 twice) — they became frequent the
		// moment the V4 calibration made band 3 reachable.
		shouldSend = notifyAdjust
		tmpl = "🛡 <b>Стоп-радар:</b> бот #{{bot_number}} {{symbol}} — band {{band}} (score {{score}}), total {{total}}"
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
	// Fallback hardening: an event whose vars don't cover the template's
	// placeholders must never ship a literal {{...}} to the operator (prod
	// 2026-09-01: the generic fallback rendered "Уведомление: {{message}}"
	// twice on radar band transitions). Compose a readable line instead.
	if strings.Contains(rendered, "{{") {
		parts := make([]string, 0, len(vars))
		for k, v := range vars {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(parts)
		rendered = fmt.Sprintf("🔔 <b>%s:</b> %s", eventType, strings.Join(parts, ", "))
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

// recordCandidateOutcome back-fills the deployed candidate's row with the
// bot's final result (v2.0.54): entry features and outcomes then live in
// one table, and score calibration no longer depends on a fragile
// candidate_id JOIN through bot rows. First outcome wins (outcome_at IS
// NULL guard) — a re-entered symbol writes its own new candidate row.
func recordCandidateOutcome(ctx context.Context, db *pgxpool.Pool, candidateID *string, total decimal.Decimal, reason string) {
	if candidateID == nil || strings.TrimSpace(*candidateID) == "" {
		return
	}
	_, _ = db.Exec(ctx, `
		UPDATE autogrid_candidates
		SET outcome_pnl_usdt = $2, outcome_closed_reason = $3, outcome_at = NOW()
		WHERE id = $1 AND outcome_at IS NULL
	`, *candidateID, total, reason)
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
