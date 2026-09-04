package autogrid

import (
	"context"
	"fmt"
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
// Decision matrix (mode ACTIVE; v2.0.72 covers BOTH fleets — paper shifts
// its simulated range, REAL moves the native grid through adjust_params):
//
//	B1     observe only
//	B2     EARLY re-center when the shift is still executable: price 70%+ of
//	       the way to the adverse edge AND the profit preflight passes
//	       (v2.0.76 "shift on green" — see the preflight invariant below)
//	B3     re-center, consumes the normal adjustments budget
//	B4     escape re-center, may exceed the budget by one — it replaces a
//	       stop-loss, and a stop never asked the budget's permission
//
// v2.0.76 feasibility preflight: the exchange rejects adjust_params on any
// bot whose current profit is negative (BOT_INVALID_ARGUMENT /
// PROFIT_LESS_THAN_ZERO — the official futures-grid API spec gates the call
// on floating PnL > 0 unless keepInvestment/reinvest is used; prod SNXXX #669
// proved it live: band 3, dwell satisfied, shift refused, only Warn). Every
// radar arm therefore checks the last-reconciled remote total FIRST and a
// blocked shift becomes a durable RADAR_SHIFT_BLOCKED_UNDERWATER event (1/h
// per bot) instead of a guaranteed rejection: the exchange is never asked,
// the cooldown is not armed (no action happened) and the exit decision stays
// with the stop ladder / operator.
//
// Churn guard (v2.0.56 F5 calibration): a per-bot cooldown plus a dwell
// requirement — the signal must sit over B3 for ≥3 consecutive snapshots
// before acting. On the calibration day s1≥0.9 sat in 17.8% of all rows;
// without dwell+cooldown that saturation becomes a fee machine, with them
// the ~20 signals/adverse-day collapse to ~22 re-centers of which ~64%
// precede an adverse confirmation (2026-08-31 audit: even manual operator
// churn cost $3-8/day).
//
// v2.0.68: the cooldown is dist-aware and durable. The flat 2h window was
// sized for full-width distances; against the minute-knife class (OP
// 2026-09-02: dist_to_stop 0.15σ) it reacted hours after the stop was gone.
// The window now tracks the Brownian expected time to the barrier —
// E[T] = d²·1h at d ATR-σ — discounted by distAwareCooldownFactor and
// clamped to the original churn bounds (15m floor, 2h cap): 1.5σ → ~1.24h,
// 0.15σ → the 15m floor. The last-action source is bot_execution_events,
// not memory — a worker restart used to hand every bot a fresh window it
// had just spent.
const (
	radarDwellTicks = 3
	radarMinBotAge  = 30 * time.Minute

	radarCooldownFloor = 15 * time.Minute
	radarCooldownCap   = 2 * time.Hour
	// distAwareCooldownFactor discounts the Brownian expected time-to-stop
	// (d² hours at d ATR-σ): act at ~55% of expected survival, before the
	// first-passage mass arrives at the barrier.
	distAwareCooldownFactor = 0.55

	// radarB2EarlyEdgeProgressMin is the minimum share of the range width the
	// price must have covered toward the ADVERSE edge before the band-2 early
	// re-center may fire: below it the bot is not actually near its danger
	// side and a shift is churn, above it the B3 trigger is imminent.
	radarB2EarlyEdgeProgressMin = 0.70
)

// adjustShiftProfitFloor is the local feasibility floor for any native
// adjust_params range shift. The exchange gate is floating PnL > 0
// (PROFIT_LESS_THAN_ZERO); the local proxy is the remote total
// (realized+floating from the last reconcile), and the +$0.10 buffer absorbs
// drift between the reconcile and the shift attempt so a borderline bot is
// not shipped into a guaranteed rejection.
var adjustShiftProfitFloor = decimal.NewFromFloat(0.10)

// adjustShiftFeasible reports whether a native range shift is locally
// executable. It is the single preflight both the radar arms and the manage
// RANGE_BREAK path must clear before touching adjust_params — calling the
// exchange below the floor can only produce PROFIT_LESS_THAN_ZERO.
func adjustShiftFeasible(remoteTotal decimal.Decimal) bool {
	return remoteTotal.GreaterThanOrEqual(adjustShiftProfitFloor)
}

// radarB2EarlyEdgeProgress is the share of the range width the price has
// covered toward the edge the inventory fears (0..1): long inventory runs at
// the lower bound, short inventory at the upper. A mid-sitting bot (flat
// inventory) has no adverse edge and returns 0 on the side it would flee —
// the early trigger then never fires, same as no trigger at all.
func radarB2EarlyEdgeProgress(price, lower, upper decimal.Decimal, inventorySide float64) float64 {
	span := upper.Sub(lower)
	if !span.IsPositive() || !price.GreaterThan(decimal.Zero) {
		return 0
	}
	var progress float64
	if inventorySide >= 0 {
		progress, _ = upper.Sub(price).Div(span).Float64()
	} else {
		progress, _ = price.Sub(lower).Div(span).Float64()
	}
	if progress < 0 {
		return 0
	}
	if progress > 1 {
		return 1
	}
	return progress
}

// radarActionCooldownFor is the dist-aware re-center window for one score:
// clamp(distAwareCooldownFactor·d²·1h, 15m, 2h) with d = dist_to_stop in
// ATR-σ. d ≤ 0 means the price is at or through the barrier (s1 = 1) —
// maximum urgency, the floor.
func radarActionCooldownFor(distToStopATR float64) time.Duration {
	if distToStopATR <= 0 {
		return radarCooldownFloor
	}
	window := time.Duration(distAwareCooldownFactor * distToStopATR * distToStopATR * float64(time.Hour))
	if window > radarCooldownCap {
		return radarCooldownCap
	}
	if window < radarCooldownFloor {
		return radarCooldownFloor
	}
	return window
}

// radarLastActionAt is the durable cooldown source: the newest ADJUST_RANGE
// event whose reason marks it as a radar re-center. Manage-loop shifts log
// the same event_type with RANGE_BREAK_* reasons and must not arm this
// window. Fail-open (no row, no table) = no cooldown, same contract as the
// old in-memory miss.
func (worker *Worker) radarLastActionAt(ctx context.Context, botID string) (time.Time, bool) {
	var at *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(created_at)
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE'
		  AND details->>'reason' IN ('RADAR_B3_RECENTER', 'RADAR_B4_ESCAPE', 'RADAR_B2_EARLY_RECENTER')
	`, botID).Scan(&at); err != nil || at == nil {
		return time.Time{}, false
	}
	return *at, true
}

// radarDwellAtOrAbove counts the trailing snapshots of the bot at or above
// the threshold (current snapshot included — it is persisted before the
// action matrix runs). v2.0.75 flicker tolerance: the count is taken over a
// window of radarDwellTicks+1 snapshots and ONE sub-threshold flicker inside
// the window no longer resets it — prod AXTIX #668 alternated 2↔3↔4 at the
// band boundary and the strict-consecutive rule read dwell=1 on exactly the
// snapshots that mattered. Two misses inside the window still block: the
// signal must persist, not just recur.
func (worker *Worker) radarDwellAtOrAbove(ctx context.Context, botID string, threshold float64) int {
	rows, err := worker.db.Query(ctx, `
		SELECT score FROM bot_risk_snapshots
		WHERE bot_id = $1 ORDER BY captured_at DESC LIMIT 6
	`, botID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	window := make([]float64, 0, radarDwellTicks+1)
	for rows.Next() {
		var score decimal.Decimal
		if err := rows.Scan(&score); err != nil {
			break
		}
		f, _ := score.Float64()
		window = append(window, f)
		if len(window) == radarDwellTicks+1 {
			break
		}
	}
	n := 0
	for _, f := range window {
		if f >= threshold {
			n++
		}
	}
	return n
}

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

// radarRecenterBudgetAllows reports whether a re-center at the given band is
// still within budget. B3 consumes the normal per-bot adjustment budget;
// B4 (escape) may exceed it by exactly ONE shift — the header contract
// "may exceed the budget by one" — and never more: an unbounded B4 lane
// under the 2h cooldown is a fee machine wearing a stop-loss costume.
func radarRecenterBudgetAllows(band, adjustments, maxAdjustmentsPerBot int) bool {
	if band >= 4 {
		return adjustments < maxAdjustmentsPerBot+1
	}
	return adjustments < maxAdjustmentsPerBot
}

// radarShiftBlockedUnderwater records the durable no-action decision when the
// profit preflight vetoes a radar re-center: adjust_params on a bot with
// negative profit is a guaranteed PROFIT_LESS_THAN_ZERO rejection, so the
// exchange is not asked and the exit stays owned by the stop ladder or the
// operator. One RADAR_SHIFT_BLOCKED_UNDERWATER event per bot per hour
// (model_state marker, the tranche2SkipAt pattern). The radar cooldown is
// deliberately NOT armed — no action happened — so the shift becomes
// eligible again the moment profit recovers past the floor.
func (worker *Worker) radarShiftBlockedUnderwater(ctx context.Context, b radarInput, rs radarScores, total decimal.Decimal) {
	table := "grid_bots"
	if b.botSource == "PAPER" {
		table = "paper_grid_bots"
	}
	tag, err := worker.db.Exec(ctx, `
		UPDATE `+table+`
		SET model_state = jsonb_set(COALESCE(model_state, '{}'::jsonb), '{radarShiftBlockedAt}',
			to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
		    updated_at = NOW()
		WHERE id = $1
		  AND COALESCE((model_state->>'radarShiftBlockedAt')::TIMESTAMPTZ, '1970-01-01') < NOW() - INTERVAL '1 hour'
	`, b.botID)
	if err != nil {
		worker.logger.Warn("stop-radar: shift-blocked marker write failed",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
		return
	}
	if tag.RowsAffected() != 1 {
		return // deduped: the hour-old blocked event still speaks for this state
	}
	_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
		"RADAR_SHIFT_BLOCKED_UNDERWATER", &b.price, &total, map[string]any{
			"band": rs.Band, "score": decimal.NewFromFloat(rs.Score).Round(4).String(),
			"total_pnl": total.StringFixed(4),
			"reason":    "PROFIT_LESS_THAN_ZERO_PREFLIGHT",
		})
	_ = QueueTelegramEvent(ctx, worker.db, "RADAR_SHIFT_BLOCKED_UNDERWATER", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol, "band": rs.Band,
		"score": decimal.NewFromFloat(rs.Score).Round(3).String(), "total": total.StringFixed(2),
	})
}

// radarMaybeRecenter executes the B2-early/B3/B4 action matrix for one bot.
func (worker *Worker) radarMaybeRecenter(ctx context.Context, settings Settings, b radarInput, rs radarScores) {
	if settings.StopForecastMode != "ACTIVE" || rs.Band < 2 {
		return
	}

	// v2.0.76 "shift on green": band 2 arms the EARLY preventive re-center —
	// but only when the price has already covered 70%+ of the way to the
	// edge the inventory fears. B3 typically fires with the price at the stop
	// and the bot under water, which the exchange refuses to shift; the B2
	// window is exactly the span where the re-center is still executable.
	early := rs.Band == 2
	if early && radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide) < radarB2EarlyEdgeProgressMin {
		return
	}

	// Durable dist-aware cooldown: blocked while the bot's last radar
	// re-center (bot_execution_events) is younger than the window its
	// current dist_to_stop demands — restart-proof, and a 0.15σ knife waits
	// minutes, not the flat 2h.
	if lastAt, ok := worker.radarLastActionAt(ctx, b.botID); ok &&
		time.Since(lastAt) < radarActionCooldownFor(rs.DistToStopATR) {
		return
	}

	// v2.0.56 (F5) dwell gate: act only on a signal that has persisted for
	// ≥3 snapshots over the band's own threshold (B2 early dwells over B2,
	// B3/B4 over B3) — a single spike is noise. Checked after the cooldown
	// so the snapshot query only runs for cooldown-eligible bots.
	dwellThreshold := bandB3
	if early {
		dwellThreshold = bandB2
	}
	if worker.radarDwellAtOrAbove(ctx, b.botID, dwellThreshold) < radarDwellTicks {
		return
	}

	// v2.0.72: REAL bots join the action matrix. Shared gates above (mode,
	// band, durable cooldown, dwell) already ran; the arm differs only in
	// execution — native adjust_params instead of a simulated range UPDATE.
	if b.botSource == "REAL" {
		worker.radarRecenterReal(ctx, settings, b, rs, early)
		return
	}

	type actBot struct {
		direction            string
		entry, investment    decimal.Decimal
		lower, upper         decimal.Decimal
		realized, unrealized decimal.Decimal
		leverage, gridNum    int
		lastLevel            *int
		antiHunt             *decimal.Decimal
		adjustments          int
		openedAt             time.Time
	}
	var bot actBot
	err := worker.db.QueryRow(ctx, `
		SELECT direction, entry_price, quote_investment,
		       lower_price, upper_price, realized_pnl_usdt, unrealized_pnl_usdt,
		       leverage, grid_num, last_grid_level, anti_hunt_stop_price,
		       COALESCE(adjustments_count, 0), opened_at
		FROM paper_grid_bots
		WHERE id = $1 AND status = 'RUNNING'
	`, b.botID).Scan(
		&bot.direction, &bot.entry, &bot.investment,
		&bot.lower, &bot.upper, &bot.realized, &bot.unrealized,
		&bot.leverage, &bot.gridNum, &bot.lastLevel, &bot.antiHunt,
		&bot.adjustments, &bot.openedAt,
	)
	if err != nil {
		return // not RUNNING (a close won the race) or already gone
	}
	// v2.0.56 (F5): a bot younger than 30 minutes has no radar history worth
	// trusting — its distance-to-stop picture is entry noise, not regime.
	if time.Since(bot.openedAt) < radarMinBotAge {
		return
	}
	if !radarRecenterBudgetAllows(rs.Band, bot.adjustments, settings.MaxAdjustmentsPerBot) {
		return // budget spent: B3 at max; B4 after its single escape slot
	}
	if !b.price.GreaterThan(decimal.Zero) || !bot.upper.GreaterThan(bot.lower) {
		return
	}

	// v2.0.76 feasibility preflight, paper arm: the simulated shift must obey
	// the same green-only policy the exchange enforces on REAL, or paper
	// calibration keeps endorsing maneuvers the native fleet cannot execute.
	if paperTotal := bot.realized.Add(bot.unrealized); !adjustShiftFeasible(paperTotal) {
		worker.radarShiftBlockedUnderwater(ctx, b, rs, paperTotal)
		return
	}

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
	// Placeholders are numbered dynamically — a NEUTRAL bot skips the entry
	// clause, and a hardcoded $8 on the stop would send pgx a parameter
	// mismatch (the exact bug that ate every v2.0.52 re-center on this
	// all-NEUTRAL fleet).
	setClauses := ""
	var extraArgs []any
	if bot.direction != "NEUTRAL" {
		setClauses += fmt.Sprintf(", entry_price = $%d", 7+len(extraArgs))
		extraArgs = append(extraArgs, b.price)
	}
	if bot.antiHunt != nil && bot.antiHunt.GreaterThan(decimal.Zero) {
		newStop := recenterStop(bot.direction, bot.lower, bot.upper, newLower, newUpper, *bot.antiHunt)
		setClauses += fmt.Sprintf(", anti_hunt_stop_price = $%d", 7+len(extraArgs))
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
	if early {
		reason = "RADAR_B2_EARLY_RECENTER"
	}
	total := bot.realized.Add(bot.unrealized)
	worker.logger.Info("stop-radar recentered grid",
		"component", "autogrid_worker", "symbol", b.symbol,
		"band", rs.Band, "score", decimal.NewFromFloat(rs.Score).Round(4).String(),
		"lower", newLower.String(), "upper", newUpper.String())

	// The event insert below ARMS the durable cooldown — a swallowed error
	// here would leave the next re-center un-gated on the following pass.
	if err := LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "PAPER", b.symbol, "ADJUST_RANGE", &b.price, &total, map[string]any{
		"reason": reason, "action": "RADAR_RECENTER",
		"score":     decimal.NewFromFloat(rs.Score).Round(4).String(),
		"band":      rs.Band,
		"s1":        decimal.NewFromFloat(rs.S1).Round(3).String(),
		"s2":        decimal.NewFromFloat(rs.S2).Round(3).String(),
		"s3":        decimal.NewFromFloat(rs.S3).Round(3).String(),
		"s4":        decimal.NewFromFloat(rs.S4).Round(3).String(),
		"new_lower": newLower.String(), "new_upper": newUpper.String(),
	}); err != nil {
		worker.logger.Warn("stop-radar: recenter event insert failed — durable cooldown not armed",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
	}
	if early {
		queueRadarB2EarlyTelegram(ctx, worker, b, rs, newLower, newUpper)
		return
	}
	_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol,
		"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
		"reason": reason, "score": decimal.NewFromFloat(rs.Score).Round(3).String(),
	})
}

// queueRadarB2EarlyTelegram renders the early-re-center notification with its
// own template: a preventive green-window shift must be distinguishable from
// a B3/B4 escape in the operator's feed, not folded into the generic range
// adjust line.
func queueRadarB2EarlyTelegram(ctx context.Context, worker *Worker, b radarInput, rs radarScores, newLower, newUpper decimal.Decimal) {
	progress := radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide)
	_ = QueueTelegramEvent(ctx, worker.db, "RADAR_B2_EARLY_RECENTER", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol,
		"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
		"score":              decimal.NewFromFloat(rs.Score).Round(3).String(),
		"edge_progress_pct":  decimal.NewFromFloat(progress * 100).Round(0).String(),
		"total":              b.total.StringFixed(2),
	})
}

// radarRecenterReal is the REAL arm of the radar action matrix (v2.0.72):
// the same B2-early/B3/B4 geometry as paper (recenterBounds), executed
// through Service.AdjustBot mode=adjust_params — the identical path the
// manage loop's tranche/range shifts and the operator console use, so the
// exchange-side contract (live openPrice, row = existing grid_num,
// status=RUNNING + settings guard) cannot drift between callers.
//
// Budget sharing with the manage path is structural, not duplicated: both
// read and increment the same grid_bots.adjustments_count, so a manage
// RANGE_BREAK shift and a radar re-center draw from one per-bot budget;
// the durable cooldown below arms on bot_execution_events by bot_id, where
// REAL ids already flow.
func (worker *Worker) radarRecenterReal(ctx context.Context, settings Settings, b radarInput, rs radarScores, early bool) {
	type realBot struct {
		direction    string
		lower, upper decimal.Decimal
		gridNum      int
		antiHunt     *decimal.Decimal
		adjustments  int
		total        decimal.Decimal
		createdAt    time.Time
	}
	var bot realBot
	err := worker.db.QueryRow(ctx, `
		SELECT direction, lower_price, upper_price, grid_num,
		       NULLIF(anti_hunt_stop_price, 0),
		       COALESCE(adjustments_count, 0), created_at,
		       COALESCE(realized_pnl_usdt, 0) + COALESCE(unrealized_pnl_usdt, 0)
		FROM grid_bots
		WHERE id = $1 AND autogrid_settings_id = $2
		  AND bu_order_id IS NOT NULL AND status = 'RUNNING'
	`, b.botID, settings.ID).Scan(
		&bot.direction, &bot.lower, &bot.upper, &bot.gridNum,
		&bot.antiHunt, &bot.adjustments, &bot.createdAt, &bot.total,
	)
	if err != nil {
		return // not RUNNING (a stop won the race) or already gone
	}
	// Same trust gate as paper: a bot younger than 30 minutes has no radar
	// history worth acting on.
	if time.Since(bot.createdAt) < radarMinBotAge {
		return
	}
	if !radarRecenterBudgetAllows(rs.Band, bot.adjustments, settings.MaxAdjustmentsPerBot) {
		return // budget spent: B3 at max; B4 after its single escape slot
	}
	if !b.price.GreaterThan(decimal.Zero) || !bot.upper.GreaterThan(bot.lower) {
		return
	}

	newLower, newUpper := recenterBounds(bot.lower, bot.upper, b.price)

	// v2.0.76 feasibility preflight: the last-reconciled remote total must
	// clear the profit floor or the exchange rejects adjust_params with
	// BOT_INVALID_ARGUMENT/PROFIT_LESS_THAN_ZERO (prod SNXXX #669: band 3,
	// dwell satisfied, shift refused 08:12:48Z 09-04 — the only successful
	// REAL shifts in the ledger came from bots in profit). A blocked shift
	// becomes the durable RADAR_SHIFT_BLOCKED_UNDERWATER decision instead of
	// a guaranteed rejection; the exit stays with the stop ladder/operator.
	if !adjustShiftFeasible(bot.total) {
		worker.radarShiftBlockedUnderwater(ctx, b, rs, bot.total)
		return
	}

	// AdjustBot increments adjustments_count only AFTER the native
	// adjustParams succeeds — a failed adjust returns here with the budget
	// and geometry untouched (Warn, never a phantom shift).
	if _, err := worker.service.AdjustBot(ctx, worker.accounts, settings.ID, b.botID, AdjustBotInput{
		Mode:  "adjust_params",
		Lower: newLower,
		Upper: newUpper,
		Row:   bot.gridNum,
	}); err != nil {
		worker.logger.Warn("stop-radar: REAL recenter adjust_params failed — budget untouched",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
		// v2.0.75 deaf-branch fix: the exchange rejecting adjustParams used
		// to leave ONLY this Warn — no event, no telegram, no backoff, so the
		// radar retried invisibly forever while the operator saw "радар
		// молчал" (prod AXTIX #668: band-4 with satisfied dwell for 8h, zero
		// actions, zero traces). One durable RADAR_RECENTER_FAILED event per
		// hour (model_state marker, the tranche2SkipAt pattern) + a telegram
		// line — the failure becomes a first-class signal.
		tag, markErr := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET model_state = jsonb_set(COALESCE(model_state, '{}'::jsonb), '{radarFailAlertAt}',
				to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
			    updated_at = NOW()
			WHERE id = $1
			  AND COALESCE((model_state->>'radarFailAlertAt')::TIMESTAMPTZ, '1970-01-01') < NOW() - INTERVAL '1 hour'
		`, b.botID)
		if markErr == nil && tag.RowsAffected() == 1 {
			_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "REAL", b.symbol,
				"RADAR_RECENTER_FAILED", &b.price, nil, map[string]any{
					"band": rs.Band, "score": decimal.NewFromFloat(rs.Score).Round(4).String(),
					"error":          err.Error(),
					"proposed_lower": newLower.String(), "proposed_upper": newUpper.String(),
				})
			_ = QueueTelegramEvent(ctx, worker.db, "RADAR_RECENTER_FAILED", map[string]any{
				"bot_number": b.botNumber, "symbol": b.symbol, "band": rs.Band,
				"error": err.Error(),
			})
		}
		return
	}

	// The exchange stop is immutable through adjustParams, so the local
	// anti-hunt must travel with the range exactly like the manage path's
	// shift — a stale stop would keep gating the OLD bounds and protect
	// nothing.
	if bot.antiHunt != nil && bot.antiHunt.GreaterThan(decimal.Zero) {
		newStop := recenterStop(bot.direction, bot.lower, bot.upper, newLower, newUpper, *bot.antiHunt)
		if _, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET anti_hunt_stop_price = $2, updated_at = NOW()
			WHERE id = $1
		`, b.botID, newStop); err != nil {
			worker.logger.Warn("stop-radar: REAL anti-hunt re-anchor failed",
				"component", "autogrid_worker", "symbol", b.symbol, "error", err)
		}
	}

	reason := "RADAR_B3_RECENTER"
	if rs.Band >= 4 {
		reason = "RADAR_B4_ESCAPE"
	}
	if early {
		reason = "RADAR_B2_EARLY_RECENTER"
	}
	worker.logger.Info("stop-radar recentered REAL grid",
		"component", "autogrid_worker", "symbol", b.symbol,
		"band", rs.Band, "score", decimal.NewFromFloat(rs.Score).Round(4).String(),
		"lower", newLower.String(), "upper", newUpper.String())

	// The event insert below ARMS the durable cooldown — a swallowed error
	// here would leave the next re-center un-gated on the following pass.
	if err := LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "REAL", b.symbol, "ADJUST_RANGE", &b.price, &bot.total, map[string]any{
		"reason": reason, "action": "RADAR_RECENTER",
		"score":     decimal.NewFromFloat(rs.Score).Round(4).String(),
		"band":      rs.Band,
		"s1":        decimal.NewFromFloat(rs.S1).Round(3).String(),
		"s2":        decimal.NewFromFloat(rs.S2).Round(3).String(),
		"s3":        decimal.NewFromFloat(rs.S3).Round(3).String(),
		"s4":        decimal.NewFromFloat(rs.S4).Round(3).String(),
		"new_lower": newLower.String(), "new_upper": newUpper.String(),
	}); err != nil {
		worker.logger.Warn("stop-radar: REAL recenter event insert failed — durable cooldown not armed",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
	}
	if early {
		queueRadarB2EarlyTelegram(ctx, worker, b, rs, newLower, newUpper)
		return
	}
	_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol,
		"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
		"reason": reason, "score": decimal.NewFromFloat(rs.Score).Round(3).String(),
		"adjustments_count": bot.adjustments + 1,
	})
}
