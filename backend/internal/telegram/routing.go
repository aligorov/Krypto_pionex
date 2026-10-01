package telegram

func EventRouting(eventType string, ch Settings) (shouldSend bool, tmpl string) {
	switch eventType {
	case "TERMINAL_FINAL_CORRECTED":
		shouldSend = true
		tmpl = "✅ <b>Биржевой итог уточнён:</b> бот #{{bot_number}} {{symbol}}\nПредварительно: {{estimate_was}} USDT\nПодтверждено биржей: {{exchange_final}} USDT"
	case "FUNDING_STALE", "FUNDING_UNAVAILABLE", "CAPITAL_DEFICIENCY", "EQUITY_CAPTURE_FAILED", "WS_RATE_LIMIT", "LIQ_FEED_FAILOVER":
		shouldSend = ch.NotifyEmergency
		tmpl = "⚠️ <b>Данные и капитал:</b> {{message}}"
	case "BOT_CREATED":
		shouldSend = ch.NotifyBotCreated
		tmpl = ch.TemplateBotCreated
	case "TAKE_PROFIT":
		shouldSend = ch.NotifyTakeProfit
		tmpl = ch.TemplateTakeProfit
	case "STOP_LOSS":
		shouldSend = ch.NotifyStopLoss
		tmpl = ch.TemplateStopLoss
	case "DELIST_SWEEP", "TRANCHE_2":
		// v2.0.19: shouldSend was never set in this branch (var defaults to
		// false), so sweep closes and tranche-2 top-ups were silently
		// dropped from Telegram — operators learned nothing while bots
		// closed "by themselves".
		shouldSend = ch.NotifyRangeAdjust
		tmpl = ch.TemplateRangeAdjust // same rendering slot as range shifts; vars overlap
	case "TRANCHE_2_SKIPPED":
		// v2.0.56 F2: risk-gated top-up skip — operator must see WHY a bot
		// stays on its first tranche (cap $12 / fleet envelope).
		shouldSend = ch.NotifyRangeAdjust
		tmpl = "⛔ <b>Транш-2 отложен:</b> бот #{{bot_number}} {{symbol}} — {{reason}}"
	case "RADAR_RECENTER_FAILED":
		// v2.0.75: the exchange refusing the radar's escape adjust used to be
		// a Warn-only swallow — the operator saw a silent radar while bots sat
		// in band 4 for hours. One line per hour, first-class signal.
		shouldSend = ch.NotifyRangeAdjust
		tmpl = "⚠️ <b>Радар: эскейп отклонён биржей:</b> бот #{{bot_number}} {{symbol}} band {{band}} — {{error}}"
	case "RADAR_B2_EARLY_RECENTER":
		// v2.0.76 "shift on green": the preventive band-2 re-center fired
		// while the profit preflight still passes — the classic dwell-3
		// window (v2.0.85: under water the shift now ships keepInvestment
		// instead of being blocked; see RADAR_B2_VELOCITY_RECENTER for the
		// one-tick lane).
		shouldSend = ch.NotifyRangeAdjust
		tmpl = "🛡 <b>Радар: ранний ре-центр на зелёном (B2):</b> бот #{{bot_number}} {{symbol}} — цена прошла {{edge_progress_pct}}% пути к опасному краю, score {{score}}, total {{total}} USDT → [{{lower_price}}, {{upper_price}}]"
	case "RADAR_B2_VELOCITY_RECENTER":
		// v2.0.85 "shift early": the trajectory lane — from 55% of the way
		// to the adverse edge, a price racing at ≥ 0.6×ATR(15м)/15м fires
		// after ONE tick (dwell 1); the dwell-3 early window slips past on
		// exactly these pairs. Requires a still-green base (normal shift).
		shouldSend = ch.NotifyRangeAdjust
		tmpl = "⚡ <b>Радар: скоростной ре-центр (B2 velocity):</b> бот #{{bot_number}} {{symbol}} — цена на {{edge_progress_pct}}% пути к краю, скорость {{speed_atr_15m}}×ATR/15м, score {{score}}, total {{total}} USDT → [{{lower_price}}, {{upper_price}}]"
	case "RADAR_AUTOCLOSE":
		// v2.0.84: the radar closed the bot itself (opt-in mode BAND3/STRICT).
		// Queued BEFORE the close intent — the operator must see the why
		// (band, score, total at the moment) even when the native stop wins
		// the race.
		shouldSend = ch.NotifyStopLoss
		tmpl = "🛑 <b>Радар: автозакрытие ({{mode}}):</b> бот #{{bot_number}} {{symbol}} — band {{band}} (score {{score}}), total {{total}} USDT — {{reason}}"
	case "STOP_FORECAST_SHADOW":
		// v2.0.57: the radar's band transitions used to fall into the
		// generic {{message}} fallback and shipped literal placeholders
		// (prod 2026-09-01: XLM band 2/3 twice) — they became frequent the
		// moment the V4 calibration made band 3 reachable.
		shouldSend = ch.NotifyRangeAdjust
		tmpl = "🛡 <b>Стоп-радар:</b> бот #{{bot_number}} {{symbol}} — band {{band}} (score {{score}}), total {{total}}"
	case "DGT_REDEPLOY":
		// v2.0.89 part B: the DGT break re-start fired — the grid follows the
		// market (arXiv 2506.11921). Rides the bot-created channel: a
		// new bot exists on the fleet.
		shouldSend = ch.NotifyBotCreated
		tmpl = "🔄 <b>DGT: пробой — сетка перезапущена центром {{center_price}}</b>, символ {{symbol}} (бот #{{bot_number}}, бюджет {{budget}} USDT, диапазон [{{lower_price}}, {{upper_price}}])"
	case "DIRECTION_FLIP":
		// v2.0.140 (package E): the first direction-flip experiment — a
		// NEUTRAL slot that died on a confirmed DOWN-side break mid-cascade
		// does not re-center into the same knife; the scanner's SHORT-cascade
		// lane takes the slot.
		// v2.0.143 (audit-2): shouldSend is now UNCONDITIONAL. The flip used
		// to ride the adjust channel (notify_range_adjust) — but a REAL
		// direction reversal moving real money must never be silencable by
		// the range-shift toggle an operator flips to quiet routine grid
		// maintenance. Same always-on semantics as the generic default lane.
		shouldSend = true
		tmpl = "🔁 <b>Флип направления (E):</b> бот #{{bot_number}} {{symbol}} — NEUTRAL закрыт по {{trigger}}, дамп подтверждён ({{ofi_regime}}), каскад ликвидаций {{cascade_usd}}/ч — слот уходит сканеру под SHORT-каскад"
	case "GRID_AGED_HALF_LIFE":
		// v2.0.89 part B: the OU half-life rotation — the fitted range
		// statistically decayed; the slot returns to the scanner. A planned
		// exit, not an alarm: the adjust channel carries it.
		shouldSend = ch.NotifyRangeAdjust
		tmpl = "⏳ <b>Half-life ротация:</b> бот #{{bot_number}} {{symbol}} ({{bot_source}}) — возраст {{age_hours}}ч превысил {{max_age_hours}}ч (OU HL {{half_life_hours}}ч), слот свободен для сканера"
	case "RANGE_ADJUST", "ADJUST_RANGE":
		shouldSend = ch.NotifyRangeAdjust
		tmpl = ch.TemplateRangeAdjust
	case "DIGEST":
		shouldSend = ch.NotifyDigest
		tmpl = ch.TemplateDigest
	case "EMERGENCY":
		shouldSend = ch.NotifyEmergency
		tmpl = "🚨 <b>EMERGENCY ALERT</b>\n{{message}}"
	case "EMERGENCY_OFI_DUMP", "EMERGENCY_OFI_PUMP":
		// v2.0.143 (audit-2): the OFI protective exits used to fall into the
		// generic default lane (shouldSend = true), bypassing the
		// notify_emergency toggle — the one channel an operator expects to
		// control stayed always-on for exactly these alarms. They now ride
		// the emergency channel like every other EMERGENCY_* event; the 🚨
		// body arrives in vars["message"], supplied by protection.go.
		shouldSend = ch.NotifyEmergency
		tmpl = "🚨 <b>Экстренный выход (OFI)</b>\n{{message}}"
	default:
		shouldSend = true
		tmpl = "🔔 <b>Уведомление:</b> {{message}}"
	}
	return shouldSend, tmpl
}
