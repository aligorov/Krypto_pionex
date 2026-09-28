package autogrid

import (
	"context"
	"fmt"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// v2.0.149 (exit-path audit F1): the 2026-09-25..27 overshoot class — SUI
// −$4.84/$4, DOT −$5.15/$4, ORDI −$10.45/$8, NEAR −$14.90/$8, four of four
// local stops past the cap — grew from supervision losing its price feed
// without a word. A dead priceMap made every manage pass skip every bot
// (`if price.IsZero() { continue }`) while the zero unrealized that leaves
// behind keeps being persisted, masking the loss. The three helpers here
// close that hole:
//
//   - blindFloatingEstimate: the exchange's own total (grid + floating,
//     no local price needed) may still arm the PnL stops — accepted only
//     as a loss signal, never as a flattering positive;
//   - blindStopsEvaluate: the pure-PnL slice of decideBotAction (max-loss,
//     take-profit, trailing) keeps firing with a zero price — the price
//     exits (anti-hunt, range break) hold by design on a zero price;
//   - notePriceFeedMapHealth / notePriceFeedBotBlind: the outage becomes a
//     first-class incident — one Error per episode, an EMERGENCY Telegram
//     page after 10 blind minutes, repeated at most hourly.

// blindFloatingEstimate derives the floating PnL leg from the exchange's
// own running total. TotalProfit ≈ realized(grid) + floating on the same
// basis the supervision floor uses, so floating ≈ total − realized. The
// signal is accepted ONLY when both the total and the derived leg are
// negative: an unverifiable positive would flatter the books, and a total
// that is merely absent (zero) must never be read as an invented loss
// against booked grid profit. Zero return = no honest signal.
//
// Known conservative skew (review P3-2): for a range-shifted bot the
// exchange resets its total while the local realized keeps the pre-shift
// profit via shiftRealizedBase, so the subtraction counts that profit
// against the leg and the blind total sits BELOW the sighted floor — a
// max-loss may trip before the cap. Early-stop-only, never late and never
// positive: within the loss-signal asymmetry this function promises.
func blindFloatingEstimate(remoteTotal, realized decimal.Decimal) decimal.Decimal {
	if remoteTotal.IsNegative() {
		if est := remoteTotal.Sub(realized); est.IsNegative() {
			return est
		}
	}
	return decimal.Zero
}

// blindEstimatePlausible bounds the blind loss signal at the bot's whole
// notional (v2.0.155, review SEC-001): a floating loss beyond investment ×
// leverage is not a market move — it is a glitch or a poisoned response,
// and the blind lane must reject it (the exchange's own LossStop keeps
// guarding the position regardless).
func blindEstimatePlausible(est, investment decimal.Decimal, leverage int) bool {
	if leverage < 1 {
		leverage = 1
	}
	bound := investment.Mul(decimal.NewFromInt(int64(leverage)))
	return !est.LessThan(bound.Neg())
}

// blindStopsEvaluate runs the price-independent exits for one bot whose
// price is missing this pass. Only CLOSE_STOP_LOSS / CLOSE_TAKE_PROFIT can
// fire: decideBotAction holds the anti-hunt and range-break branches on a
// zero CurrentPrice by construction, and no shift/adjust/pour path is taken
// blind. The intent rides the same durable machinery as a sighted stop
// (recordCloseIntent → cancelRealBot), with a price_feed_blind witness in
// the event journal and the Telegram page.
func (worker *Worker) blindStopsEvaluate(ctx context.Context, client *pionex.Client, settings *Settings, bot managedBot, realized, floor decimal.Decimal) {
	if bot.localStatus != "RUNNING" {
		return
	}
	botTarget, botMaxLoss := settings.PnLTargetUSDT, settings.MaxLossUSDT
	if bot.pnlTarget != nil {
		botTarget = *bot.pnlTarget
	}
	if bot.maxLoss != nil {
		botMaxLoss = *bot.maxLoss
	}
	peakFloorNow := realized.Add(floor)
	if bot.peakFloor != nil && bot.peakFloor.GreaterThan(peakFloorNow) {
		peakFloorNow = *bot.peakFloor
	}
	decision := decideBotAction(botActionInput{
		Direction:     bot.direction,
		Lower:         bot.lower,
		Upper:         bot.upper,
		CurrentPrice:  decimal.Zero, // price exits hold on zero by design
		RealizedPNL:   realized,
		UnrealizedPNL: floor,
		PeakPNL:       peakFloorNow,
		Budget:        bot.investment,
		PnLTarget:     botTarget,
		MaxLoss:       botMaxLoss,
		Regime:        "",
	})
	if decision.Action != ActionCloseStopLoss && decision.Action != ActionCloseTakeProfit {
		return
	}
	totalPnL := realized.Add(floor)
	active, err := worker.recordCloseIntent(ctx, bot.id, decision.Reason, totalPnL)
	if err != nil {
		// Deliberately no cancel without the persisted intent (the sighted
		// path retries the whole decision next pass; an unpersisted cancel
		// would lose the EXIT_SLIPPAGE baseline this close exists to carry).
		worker.logger.Error("blind stop: close intent persist failed",
			"component", "autogrid_worker", "bot_id", bot.id, "error", err)
		return
	}
	if !active {
		return // another writer already made the bot terminal
	}
	if err := worker.cancelRealBot(ctx, client, bot.id, bot.remoteID, "autogrid "+decision.Reason); err != nil {
		worker.logger.Error("blind stop: native cancel failed",
			"component", "autogrid_worker", "bot_id", bot.id,
			"reason", decision.Reason, "error", err)
		return
	}
	eventType := "STOP_LOSS"
	if decision.Action == ActionCloseTakeProfit {
		eventType = "TAKE_PROFIT"
	}
	pnlPct := decimal.Zero
	if !bot.investment.IsZero() {
		pnlPct = totalPnL.Div(bot.investment).Mul(decimal.NewFromInt(100)).Round(2)
	}
	worker.logger.Error("PRICE FEED BLIND: PnL stop fired without a local price",
		"component", "autogrid_worker", "bot_number", bot.botNumber, "symbol", bot.symbol,
		"reason", decision.Reason, "total_pnl", totalPnL.StringFixed(4))
	_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol,
		eventType, nil, &totalPnL, map[string]any{
			"reason": decision.Reason, "pnlPct": pnlPct,
			"price_feed_blind":  true,
			"floating_source":   "exchange_total",
			"stop_intent_total": totalPnL.StringFixed(4),
		})
	_ = QueueTelegramEvent(ctx, worker.db, eventType, map[string]any{
		"bot_number": bot.botNumber, "symbol": bot.symbol,
		"pnl_usdt": totalPnL.StringFixed(4), "pnl_pct": pnlPct.StringFixed(2),
		"reason":  decision.Reason,
		"message": fmt.Sprintf("🚨 СТОП ВСЛЕПУЮ (%s): прайс-фид мёртв, стоп сработал по биржевому тоталу — проверь indexes/WS/tickers", decision.Reason),
	})
}

// notePriceFeedMapHealth records the outcome of the per-pass price map
// build. A healthy map alone must NOT close the episode: a partial outage
// (map fine, some bot symbols missing — the delisting/indexes-gap class)
// stays blind through the bots loop, and closing on map health alone would
// restart the episode every tick, spam the episode-start Error and keep the
// 10-minute EMERGENCY page forever unreachable (review P2-1). The episode
// clears only after a pass that was clean end-to-end — healthy map AND no
// blind bot — which notePriceFeedBotBlind flags into priceFeedBlindLastPass.
func (worker *Worker) notePriceFeedMapHealth(mapFailed bool) {
	if mapFailed {
		worker.markPriceFeedBlind("price map failed or empty")
		worker.priceFeedBlindLastPass = true
		return
	}
	if !worker.priceFeedBlindLastPass {
		worker.priceFeedBlindSince = nil
	}
	worker.priceFeedBlindLastPass = false
}

// notePriceFeedBotBlind records that at least one supervised bot had no
// price this pass (partial outage: map healthy, symbol missing) and keeps
// the episode alive through the next pass's map-health note.
func (worker *Worker) notePriceFeedBotBlind(symbol string, botNumber int) {
	worker.priceFeedBlindLastPass = true
	worker.markPriceFeedBlind(fmt.Sprintf("no price for #%d %s", botNumber, symbol))
}

func (worker *Worker) markPriceFeedBlind(detail string) {
	now := time.Now().UTC()
	if worker.priceFeedBlindSince == nil {
		started := now
		worker.priceFeedBlindSince = &started
		worker.logger.Error("price feed blind: PnL stops degraded to the exchange-total basis",
			"component", "autogrid_worker", "detail", detail)
		return
	}
	if now.Sub(*worker.priceFeedBlindSince) < 10*time.Minute ||
		!worker.priceFeedBlindLastAlarm.IsZero() && now.Sub(worker.priceFeedBlindLastAlarm) < time.Hour {
		return
	}
	worker.priceFeedBlindLastAlarm = now
	worker.logger.Error("PRICE FEED BLIND >10m — price exits disarmed, PnL stops on the exchange-total basis",
		"component", "autogrid_worker", "detail", detail,
		"blind_since", worker.priceFeedBlindSince.Format(time.RFC3339))
	if worker.db != nil {
		// v2.0.155 (review SEC-004): the EMERGENCY page is best-effort
		// telemetry — a bounded context keeps a black-holed Postgres
		// connection from parking the supervision goroutine forever.
		tgCtx, tgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer tgCancel()
		_ = QueueTelegramEvent(tgCtx, worker.db, "EMERGENCY", map[string]any{
			"message": "🚨 Прайс-фид слеп >10 мин: ценовые выходы (anti-hunt/range-break) разоружены, PnL-стопы переведены на биржевой тотал — проверь indexes/WS/tickers",
		})
	}
}
