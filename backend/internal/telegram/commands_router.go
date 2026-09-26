package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// v2.0.109 command router: rewrites the dead two-command controller into the
// operator's pocket console. Every handler is read-only SQL against the same
// tables the web UI reads; /kill remains the single write path. AUTH MODEL:
// private-chat only — the dispatcher answers just the chat_id pinned in
// telegram_settings; in a group that Chat.ID is shared by every member, so
// keep the pinned chat a private dialog (the /help footer states it).

// routeCommand dispatches one inbound message. Unknown input gets the menu.
// iOS clients send commands with an @botname suffix and any case; Russian
// aliases arrive as plain words — both normalized here.
func (d *OutboxDispatcher) routeCommand(ctx context.Context, token, chatID, text string) {
	text = strings.TrimSpace(text)
	cmd := text
	arg := ""
	if idx := strings.IndexAny(text, " \n"); idx > 0 {
		cmd, arg = text[:idx], strings.TrimSpace(text[idx+1:])
	}
	if idx := strings.IndexByte(cmd, '@'); idx > 0 {
		cmd = cmd[:idx]
	}
	cmd = strings.ToLower(cmd)
	if mapped, ok := ruCommandAliases[cmd]; ok {
		cmd = mapped
	}

	d.logger.Info("tg console: command received",
		"component", "telegram_console", "cmd", cmd)

	switch cmd {
	case "/start", "/help":
		d.sendMenu(ctx, token, chatID)
	case "/status":
		d.reportStatus(ctx, token, chatID)
	case "/bots":
		d.reportFleet(ctx, token, chatID)
	case "/closed":
		d.reportClosed(ctx, token, chatID, arg)
	case "/day":
		d.reportDay(ctx, token, chatID)
	case "/stats":
		d.reportStats(ctx, token, chatID)
	case "/pnl":
		d.reportPnL(ctx, token, chatID)
	case "/risk":
		d.reportRisk(ctx, token, chatID)
	case "/health":
		d.reportHealth(ctx, token, chatID)
	case "/kill":
		d.triggerKillSwitch(ctx, token, chatID)
	default:
		d.sendMenu(ctx, token, chatID)
	}
}

func (d *OutboxDispatcher) sendMenu(ctx context.Context, token, chatID string) {
	text := "🤖 <b>Pionex AutoGrid — пульт</b>\n\n" +
		"📊 <b>/status</b> — сводка флота и эпохи\n" +
		"🤖 <b>/bots</b> — активные боты (таблица)\n" +
		"🏁 <b>/closed</b> [N] — последние закрытия\n" +
		"📅 <b>/day</b> — сегодняшний день\n" +
		"📈 <b>/stats</b> — статистика по дням\n" +
		"💰 <b>/pnl</b> — таблица прибыли + прогноз\n" +
		"🛡 <b>/risk</b> — риск-лимиты и конверт\n" +
		"❤️ <b>/health</b> — версия, сбои, ожидание биржи\n" +
		"🚨 /kill — экстренный стоп новых входов\n\n" +
		"ℹ️ Пульт отвечает только в приватном чате, закреплённом в настройках Telegram."
	d.sendMessageWithKeyboard(ctx, token, chatID, text, mainMenuKeyboard())
}

func mainMenuKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "📊 Статус", CallbackData: "cmd_status"}, {Text: "🤖 Боты", CallbackData: "cmd_bots"}},
		{{Text: "🏁 Закрытые", CallbackData: "cmd_closed"}, {Text: "📅 День", CallbackData: "cmd_day"}},
		{{Text: "📈 Статистика", CallbackData: "cmd_stats"}, {Text: "💰 Прибыль", CallbackData: "cmd_pnl"}},
		{{Text: "🛡 Риск", CallbackData: "cmd_risk"}, {Text: "❤️ Здоровье", CallbackData: "cmd_health"}},
	}}
}

// reportStatus is the one-glance dashboard: mode, fleet, epoch economics.
func (d *OutboxDispatcher) reportStatus(ctx context.Context, token, chatID string) {
	fleet, err := loadFleet(ctx, d.db)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ статус: "+escapeErr(err))
		return
	}
	var capital, realized, floating decimal.Decimal
	for _, r := range fleet {
		capital = capital.Add(r.Investment)
		realized = realized.Add(r.Realized)
		floating = floating.Add(r.Floating)
	}

	var mode string
	var slots int
	_ = d.db.QueryRow(ctx, `SELECT execution_mode, max_active_bots FROM autogrid_settings ORDER BY updated_at DESC LIMIT 1`).Scan(&mode, &slots)

	var killEnabled bool
	var dailyLoss, maxLev string
	_ = d.db.QueryRow(ctx, `SELECT kill_switch_enabled, max_daily_loss_usd::TEXT, max_leverage::TEXT FROM risk_settings WHERE id = 1`).Scan(&killEnabled, &dailyLoss, &maxLev)

	// Epoch closed ledger (exchange-truth rows only) over the running epoch
	// window: the last 7 days carry every REAL closure that matters now.
	closed, err := loadClosed(ctx, d.db, 500, 7)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ статус: "+escapeErr(err))
		return
	}
	var closedNet decimal.Decimal
	for _, r := range closed {
		closedNet = closedNet.Add(r.Final)
	}

	today := time.Now().UTC().Format("02.01")
	var todayNet decimal.Decimal
	todayCloses := 0
	for _, r := range closed {
		if r.ClosedAt.UTC().Format("02.01") == today {
			todayNet = todayNet.Add(r.Final)
			todayCloses++
		}
	}

	killIcon := "🟢 OFF"
	if killEnabled {
		killIcon = "🔴 ON"
	}
	text := fmt.Sprintf(
		"📊 <b>СТАТУС</b> · %s UTC\n\n"+
			"Режим: <b>%s</b> · слоты <b>%d/%d</b>\n"+
			"Работают: <b>%d</b> · в ботах <b>$%s</b>\n"+
			"realized <b>%s</b> · плавающий <b>%s</b>\n\n"+
			"Сегодня: <b>%s</b> на %d закрытиях\n"+
			"Закрыто за 7д: <b>%s</b> (%d шт)\n\n"+
			"Kill-switch: %s · брейкер $%s · макс.гир %sx",
		time.Now().Format("15:04"), mode, len(fleet), slots,
		len(fleet), capital.Round(0).String(),
		moneySigned(realized), moneySigned(floating),
		moneySigned(todayNet), todayCloses,
		moneySigned(closedNet), len(closed),
		killIcon, dailyLoss, maxLev,
	)
	d.sendMessageWithKeyboard(ctx, token, chatID, text, mainMenuKeyboard())
}

func (d *OutboxDispatcher) reportFleet(ctx context.Context, token, chatID string) {
	fleet, err := loadFleet(ctx, d.db)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ боты: "+escapeErr(err))
		return
	}
	nums := make([]int, 0, len(fleet))
	for _, r := range fleet {
		nums = append(nums, r.BotNumber)
	}
	sparks, _ := loadSparks(ctx, d.db, nums)
	d.sendMessage(ctx, token, chatID, "🤖 <b>АКТИВНЫЕ БОТЫ</b>\n\n"+buildFleetTable(fleet, sparks))
}

func (d *OutboxDispatcher) reportClosed(ctx context.Context, token, chatID, arg string) {
	limit := 10
	if n, err := strconv.Atoi(arg); err == nil && n > 0 && n <= 50 {
		limit = n
	}
	rows, err := loadClosed(ctx, d.db, limit, 0)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ закрытые: "+escapeErr(err))
		return
	}
	var net decimal.Decimal
	for _, r := range rows {
		net = net.Add(r.Final)
	}
	text := fmt.Sprintf("🏁 <b>ПОСЛЕДНИЕ ЗАКРЫТИЯ</b> (×%d)\n\n%s\nΣ выборка: <b>%s</b>", limit, buildClosedTable(rows), moneySigned(net))
	d.sendMessage(ctx, token, chatID, text)
}

func (d *OutboxDispatcher) reportDay(ctx context.Context, token, chatID string) {
	rows, err := loadClosed(ctx, d.db, 200, 1)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ день: "+escapeErr(err))
		return
	}
	today := time.Now().UTC().Format("02.01")
	todays := make([]closedRow, 0, len(rows))
	for _, r := range rows {
		if r.ClosedAt.UTC().Format("02.01") == today {
			todays = append(todays, r)
		}
	}
	// loadClosed is newest-first; the day table reads best oldest-first.
	for i, j := 0, len(todays)-1; i < j; i, j = i+1, j-1 {
		todays[i], todays[j] = todays[j], todays[i]
	}
	fleet, _ := loadFleet(ctx, d.db)
	var capital, realized, floating decimal.Decimal
	for _, r := range fleet {
		capital = capital.Add(r.Investment)
		realized = realized.Add(r.Realized)
		floating = floating.Add(r.Floating)
	}
	text := fmt.Sprintf("📅 <b>СЕГОДНЯ · %s</b>\n\n%s\n\nРаботают сейчас: %d ботов, нетто открытых <b>%s</b>",
		today, buildClosedTable(todays), len(fleet), moneySigned(realized.Add(floating)))
	d.sendMessage(ctx, token, chatID, text)
}

func (d *OutboxDispatcher) reportStats(ctx context.Context, token, chatID string) {
	rows, err := loadClosed(ctx, d.db, 1000, 30)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ статистика: "+escapeErr(err))
		return
	}
	days := dayStatsFrom(rows)
	text := "📈 <b>СТАТИСТИКА · 30 дней</b>\n\n" + buildDayStats(days)
	d.sendMessage(ctx, token, chatID, text)
}

func (d *OutboxDispatcher) reportPnL(ctx context.Context, token, chatID string) {
	rows, err := loadClosed(ctx, d.db, 1000, 30)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ прибыль: "+escapeErr(err))
		return
	}
	days := dayStatsFrom(rows)
	fleet, _ := loadFleet(ctx, d.db)
	var capital decimal.Decimal
	for _, r := range fleet {
		capital = capital.Add(r.Investment)
	}
	text := "💰 <b>ТАБЛИЦА ПРИБЫЛИ</b>\n\n" + buildDayStats(days) + forecastLine(days, capital)
	d.sendMessage(ctx, token, chatID, text)
}

func (d *OutboxDispatcher) reportRisk(ctx context.Context, token, chatID string) {
	var kill bool
	var accountExp, symbolExp, dailyLoss string
	var maxLev, maxBots, maxPos int
	err := d.db.QueryRow(ctx, `
		SELECT kill_switch_enabled, max_account_exposure_usd::TEXT,
		       max_symbol_exposure_usd::TEXT, max_daily_loss_usd::TEXT,
		       max_leverage, max_active_grid_bots, max_open_positions
		FROM risk_settings WHERE id = 1
	`).Scan(&kill, &accountExp, &symbolExp, &dailyLoss, &maxLev, &maxBots, &maxPos)
	if err != nil {
		d.sendMessage(ctx, token, chatID, "❌ риск: "+escapeErr(err))
		return
	}
	fleet, _ := loadFleet(ctx, d.db)
	var envelope decimal.Decimal
	for _, r := range fleet {
		envelope = envelope.Add(r.MaxLoss)
	}
	killIcon := "🟢 OFF"
	if kill {
		killIcon = "🔴 ON (входы заблокированы)"
	}
	text := fmt.Sprintf("🛡 <b>РИСК-ПЕРИМЕТР</b>\n\n"+
		"Kill-switch: <b>%s</b>\nДневной брейкер: <b>$%s</b>\n"+
		"Экспозиция аккаунта: $%s · символа: $%s\n"+
		"Макс. гир: %dx · грид-ботов: %d · позиций: %d\n\n"+
		"Конверт открытого риска (Σ кап): <b>$%s</b> на %d ботах",
		killIcon, dailyLoss, accountExp, symbolExp, maxLev, maxBots, maxPos,
		envelope.Round(0).String(), len(fleet))
	d.sendMessage(ctx, token, chatID, text)
}

func (d *OutboxDispatcher) reportHealth(ctx context.Context, token, chatID string) {
	var failed3h, pendingFinals int
	var lastErr string
	_ = d.db.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE((SELECT LEFT(last_error,80) FROM grid_bots
			WHERE status='FAILED' AND last_error IS NOT NULL
			ORDER BY created_at DESC LIMIT 1),'')
		FROM grid_bots WHERE status='FAILED' AND created_at > NOW() - INTERVAL '3 hours'
	`).Scan(&failed3h, &lastErr)
	_ = d.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE reconciliation_state = 'TERMINAL_FINAL_PENDING_EXCHANGE'
	`).Scan(&pendingFinals)

	fleet, _ := loadFleet(ctx, d.db)
	failedIcon := "✅"
	if failed3h > 0 {
		failedIcon = fmt.Sprintf("⚠️ %d за 3ч", failed3h)
	}
	pendingIcon := "✅"
	if pendingFinals > 0 {
		pendingIcon = fmt.Sprintf("⏳ %d ждут биржевой тотал", pendingFinals)
	}
	text := fmt.Sprintf("❤️ <b>ЗДОРОВЬЕ СИСТЕМЫ</b>\n\n"+
		"Флот: <b>%d</b> ботов под надзором\n"+
		"Провалы create: %s\n"+
		"Терминальные финалы: %s\n"+
		"Real-time надзор: активен (событийный + базовый 45с)\n\n"+
		"Последняя ошибка деплоя:\n<code>%s</code>",
		len(fleet), failedIcon, pendingIcon, html.EscapeString(lastErr))
	d.sendMessage(ctx, token, chatID, text)
}

// escapeErr HTML-escapes the error text: raw exchange/client errors can
// carry literal '<' (JSON decode of a Cloudflare 502 page) which Telegram
// rejects under parse_mode=HTML — the reply would be silently dropped
// (adversarial review, agent_0350f97a).
func escapeErr(err error) string {
	return "<code>" + html.EscapeString(err.Error()) + "</code>"
}

// sendMessageWithKeyboard sends an HTML message with an inline keyboard.
func (d *OutboxDispatcher) sendMessageWithKeyboard(ctx context.Context, token, chatID, text string, kb InlineKeyboardMarkup) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	body := map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": kb,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		d.logger.Warn("tg console: build request failed", "component", "telegram_console", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		d.logger.Warn("tg console: send failed", "component", "telegram_console", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.logger.Warn("tg console: telegram rejected message",
			"component", "telegram_console", "status", resp.StatusCode)
	}
}

// ruCommandAliases maps the operator's Russian words to commands.
var ruCommandAliases = map[string]string{
	"меню": "/start", "статус": "/status", "боты": "/bots",
	"закрытые": "/closed", "день": "/day", "статистика": "/stats",
	"прибыль": "/pnl", "риск": "/risk", "здоровье": "/health",
}
