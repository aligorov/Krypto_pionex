package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

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
	db                         *pgxpool.Pool
	httpClient                 *http.Client
	logger                     *slog.Logger
	lastCredsWarnAt            time.Time
	lastPollWarnAt             time.Time
	webhookDropped             bool
	webhookToken               string
	service                    *Service
	version, commit, buildTime string
}

func NewOutboxDispatcher(db *pgxpool.Pool, defaultToken, defaultChat string) *OutboxDispatcher {
	return &OutboxDispatcher{
		db: db,
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

	return "", "", "", false
}

// v2.0.109: command handling moved to the full router (commands_router.go);
// this shim keeps the poll loop's call site stable.
func (d *OutboxDispatcher) handleCommand(ctx context.Context, token, chatID, text string) {
	d.routeCommand(ctx, token, chatID, text)
}

func (d *OutboxDispatcher) acknowledgeCallback(ctx context.Context, token, queryID, denial string) {
	ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	body := map[string]any{"callback_query_id": queryID}
	if denial != "" {
		body["text"] = denial
		body["show_alert"] = true
	}
	err := callTelegram(ackCtx, d.httpClient, token, "answerCallbackQuery", body, nil)
	cancel()
	if err != nil {
		d.logger.Warn("tg console: callback acknowledgement failed", "component", "telegram_console", "error", err.Error())
	}
}

func (d *OutboxDispatcher) handleCallback(ctx context.Context, token, chatID, queryID, action string) {
	d.acknowledgeCallback(ctx, token, queryID, "")

	switch action {
	case "cmd_menu":
		d.sendMenu(ctx, token, chatID)
	case "cmd_status":
		d.reportStatus(ctx, token, chatID)
	case "cmd_bots":
		d.reportFleet(ctx, token, chatID)
	case "cmd_closed":
		d.reportClosed(ctx, token, chatID, "")
	case "cmd_day":
		d.reportDay(ctx, token, chatID)
	case "cmd_stats":
		d.reportStats(ctx, token, chatID)
	case "cmd_pnl":
		d.reportPnL(ctx, token, chatID)
	case "cmd_risk":
		d.reportRisk(ctx, token, chatID)
	case "cmd_health":
		d.reportHealth(ctx, token, chatID)
	case "cmd_kill_switch":
		d.triggerKillSwitch(ctx, token, chatID)
	default:
		d.sendMenu(ctx, token, chatID)
	}
}

func (d *OutboxDispatcher) triggerKillSwitch(ctx context.Context, token, chatID string) {
	tx, err := d.db.Begin(ctx)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ Kill Switch: "+escapeErr(err))
		return
	}
	defer tx.Rollback(ctx)
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT value='true'::jsonb FROM app_config WHERE key='telegram_write_commands_enabled'),false)`).Scan(&allowed)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ Проверка доступа: "+escapeErr(err))
		return
	}
	if !allowed {
		d.sendMessage(ctx, token, chatID, "⛔ Управляющие команды Telegram отключены в настройках проекта. Kill Switch не изменён.")
		return
	}
	tag, err := tx.Exec(ctx, `UPDATE risk_settings SET kill_switch_enabled=true,updated_at=NOW() WHERE id=1`)
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("risk settings are missing")
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(action,actor,details) VALUES ('risk.kill_switch.enable',$1,'{"channel":"telegram"}'::jsonb)`, "telegram:"+chatID)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ Kill Switch: "+escapeErr(err))
		return
	}
	d.sendMessage(ctx, token, chatID, "🚨 <b>KILL SWITCH АКТИВИРОВАН</b>\nНовые входы заблокированы. Уже работающие боты остаются под надзором.")
}

func (d *OutboxDispatcher) sendMessage(ctx context.Context, token, chatID, text string) {
	d.sendMessageWithKeyboard(ctx, token, chatID, text, InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "☰ Меню", CallbackData: "cmd_menu"}}}})
}
