package autogrid

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Radar actions — Phase B (v2.0.52). The stop-radar scores each running
// bot's probability of hitting its stop; this module turns high bands into
// the one action the research endorses: a PREEMPTIVE GRID RE-CENTER
// (arXiv 2506.11921 — static grids are zero-EV, re-centering on the break
// is the IRR 60-70% mechanism). Instead of donating the −$8 stop, the grid
// re-bases around the live price, the anti-hunt stop travels with the
// range, and the underwater inventory mark crystallizes into realized
// exactly like the manage loop's own ADJUST_RANGE shift.
//
// Decision matrix (mode ACTIVE, PAPER bots only — REAL is never
// auto-adjusted):
//
//	B1/B2  observe only (B2+ still emits the shadow event/telegram)
//	B3     re-center, consumes the normal adjustments budget
//	B4     escape re-center, may exceed the budget by one — it replaces a
//	       stop-loss, and a stop never asked the budget's permission
//
// Churn guard: one radar action per bot per radarActionCooldown. Acting on
// every indicator twitch is how an active manager becomes a fee machine
// (2026-08-31 audit: even manual operator churn cost $3-8/day).

const radarActionCooldown = 60 * time.Minute

// recenterBounds mirrors adjustDecision's geometry: same width, centered
// on the live price.
func recenterBounds(lower, upper, price decimal.Decimal) (newLower, newUpper decimal.Decimal) {
	half := upper.Sub(lower).Div(decimal.NewFromInt(2))
	return price.Sub(half), price.Add(half)
}

// recenterStop moves the anti-hunt stop with the range, preserving its
// deploy-time distance beyond the bound (same formula the manage loop's
// shift has used since v2.0.15 — a stale stop drifts away from shifted
// bounds and protects nothing).
func recenterStop(direction string, oldLower, oldUpper, newLower, newUpper, oldStop decimal.Decimal) decimal.Decimal {
	if direction == "SHORT" {
		return newUpper.Add(oldStop.Sub(oldUpper))
	}
	return newLower.Sub(oldLower.Sub(oldStop))
}

// candidateSpanPct is the grid width in % of the lower bound (0 = unknown).
func candidateSpanPct(lower, upper decimal.Decimal) float64 {
	if !lower.GreaterThan(decimal.Zero) || !upper.GreaterThan(lower) {
		return 0
	}
	span, _ := upper.Sub(lower).Div(lower).Mul(decimal.NewFromInt(100)).Float64()
	return span
}

// radarMaybeRecenter executes the B3/B4 action matrix for one bot.
func (worker *Worker) radarMaybeRecenter(ctx context.Context, settings Settings, b radarInput, rs radarScores) {
	if settings.StopForecastMode != "ACTIVE" || rs.Band < 3 {
		return
	}

	worker.radarActionMu.Lock()
	if t, ok := worker.radarActionAt[b.botID]; ok && time.Since(t) < radarActionCooldown {
		worker.radarActionMu.Unlock()
		return
	}
	worker.radarActionMu.Unlock()

	type actBot struct {
		direction            string
		entry, investment    decimal.Decimal
		lower, upper         decimal.Decimal
		realized, unrealized decimal.Decimal
		leverage, gridNum    int
		lastLevel            *int
		antiHunt             *decimal.Decimal
		adjustments          int
	}
	var bot actBot
	err := worker.db.QueryRow(ctx, `
		SELECT direction, entry_price, quote_investment,
		       lower_price, upper_price, realized_pnl_usdt, unrealized_pnl_usdt,
		       leverage, grid_num, last_grid_level, anti_hunt_stop_price,
		       COALESCE(adjustments_count, 0)
		FROM paper_grid_bots
		WHERE id = $1 AND status = 'RUNNING'
	`, b.botID).Scan(
		&bot.direction, &bot.entry, &bot.investment,
		&bot.lower, &bot.upper, &bot.realized, &bot.unrealized,
		&bot.leverage, &bot.gridNum, &bot.lastLevel, &bot.antiHunt,
		&bot.adjustments,
	)
	if err != nil {
		return // not RUNNING (a close won the race) or already gone
	}
	if rs.Band < 4 && bot.adjustments >= settings.MaxAdjustmentsPerBot {
		return // budget spent; B4 below still gets its escape slot
	}
	if !b.price.GreaterThan(decimal.Zero) || !bot.upper.GreaterThan(bot.lower) {
		return
	}

	// Claim the cooldown slot before acting so a slow pass cannot double-fire.
	worker.radarActionMu.Lock()
	worker.radarActionAt[b.botID] = time.Now().UTC()
	worker.radarActionMu.Unlock()

	newLower, newUpper := recenterBounds(bot.lower, bot.upper, b.price)

	// Crystallize the mark at the live price; a shift keeps the position,
	// so the exit-fee component goes back in (paperMarkAtPrice contract).
	markUnrealized, exitNotional := paperMarkAtPrice(paperSettleBot{
		direction: bot.direction, entry: bot.entry, investment: bot.investment,
		lower: bot.lower, upper: bot.upper, leverage: bot.leverage,
		gridNum: bot.gridNum, lastLevel: bot.lastLevel, realized: bot.realized,
	}, b.price, settings)
	feeRate := settings.FeeBps.Add(settings.SlippageBps).Div(decimal.NewFromInt(10000))
	shiftRealized := bot.realized.Add(markUnrealized)
	if exitNotional.IsPositive() {
		shiftRealized = shiftRealized.Add(exitNotional.Mul(feeRate))
	}

	newLevel := gridLevelForPrice(newLower, newUpper, bot.gridNum, b.price)

	// v2.0.15 re-anchors: directional grids re-anchor entry (their mark is
	// derived from entry each tick); the anti-hunt travels with the range.
	setClauses := ""
	var extraArgs []any
	if bot.direction != "NEUTRAL" {
		setClauses += ", entry_price = $7"
		extraArgs = append(extraArgs, b.price)
	}
	if bot.antiHunt != nil && bot.antiHunt.GreaterThan(decimal.Zero) {
		newStop := recenterStop(bot.direction, bot.lower, bot.upper, newLower, newUpper, *bot.antiHunt)
		setClauses += ", anti_hunt_stop_price = $8"
		extraArgs = append(extraArgs, newStop)
	}

	if _, err := worker.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET lower_price = $2, upper_price = $3,
		    adjustments_count = adjustments_count + 1,
		    mark_price = $4, unrealized_pnl_usdt = 0,
		    realized_pnl_usdt = $5, last_grid_level = $6`+setClauses+`
		WHERE id = $1 AND status = 'RUNNING'
	`, append([]any{b.botID, newLower, newUpper, b.price, shiftRealized, newLevel}, extraArgs...)...); err != nil {
		worker.logger.Warn("stop-radar: recenter UPDATE failed",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
		return
	}

	reason := "RADAR_B3_RECENTER"
	if rs.Band >= 4 {
		reason = "RADAR_B4_ESCAPE"
	}
	total := bot.realized.Add(bot.unrealized)
	worker.logger.Info("stop-radar recentered grid",
		"component", "autogrid_worker", "symbol", b.symbol,
		"band", rs.Band, "score", decimal.NewFromFloat(rs.Score).Round(4).String(),
		"lower", newLower.String(), "upper", newUpper.String())

	_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "PAPER", b.symbol, "ADJUST_RANGE", &b.price, &total, map[string]any{
		"reason": reason, "action": "RADAR_RECENTER",
		"score":     decimal.NewFromFloat(rs.Score).Round(4).String(),
		"band":      rs.Band,
		"s1":        decimal.NewFromFloat(rs.S1).Round(3).String(),
		"s2":        decimal.NewFromFloat(rs.S2).Round(3).String(),
		"s3":        decimal.NewFromFloat(rs.S3).Round(3).String(),
		"s4":        decimal.NewFromFloat(rs.S4).Round(3).String(),
		"new_lower": newLower.String(), "new_upper": newUpper.String(),
	})
	_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol,
		"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
		"reason": reason, "score": decimal.NewFromFloat(rs.Score).Round(3).String(),
	})
}
