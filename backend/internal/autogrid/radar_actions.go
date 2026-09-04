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
// v2.0.76 feasibility preflight → v2.0.85 "shift always, shift early". The
// exchange's normal adjust_params semantics reject any bot whose floating
// PnL ≤ 0 (BOT_INVALID_ARGUMENT / PROFIT_LESS_THAN_ZERO — the official
// futures-grid API spec gates the call on floating PnL > 0; prod SNXXX #669
// proved it live: band 3, dwell satisfied, shift refused, only Warn). v2.0.76
// answered by parking under-water bots (RADAR_SHIFT_BLOCKED_UNDERWATER). The
// operator directive for v2.0.85 is the opposite: IF a shift under water is
// impossible, THEN shift EARLY — and under water it turns out POSSIBLE too:
// the documented keepInvestment=true flag moves the range without resetting
// the investment and skips the PnL check, so the preflight now selects a MODE
// instead of vetoing (adjustShiftMode below): green → normal re-base, under
// water → keep_investment rescue transfer, dry-run validated through the
// documented adjustParamsCheck endpoint before the live call. The only
// remaining refusal is the exchange's own — it lands in the durable
// RADAR_RECENTER_FAILED path with the reason.
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

	// v2.0.85 "shift always, shift early" velocity trigger (FIX-2). The
	// band-2 early window (70% + dwell 3, ~4.5 min at the ACTIVE cadence)
	// slips past on fast pairs — the price crosses the remaining 30% of the
	// range inside the dwell. The velocity lane arms on the TRAJECTORY
	// instead of the dwell:
	//   edge_progress ≥ 55%
	//   speed toward the adverse edge ≥ 0.6 × ATR(15m) per 15 minutes
	//   floating > +$0.10 (the base is still green — this is prevention, not
	//   rescue; the underwater lane is FIX-1 keepInvestment)
	//   dwell 1 — speed does not wait
	// plus the shared cooldown/budget/age gates.
	radarB2VelocityEdgeProgressMin = 0.55
	radarB2VelocitySpeedATR15      = 0.6
	// radarVelocityFreshWindow suppresses zero-gap samples when two bots on
	// one symbol are scored in the same pass, and radarVelocityMaxAge caps
	// how stale a baseline may be before the "speed" stops meaning anything
	// (worker downtime must not fabricate velocity).
	radarVelocityFreshWindow = 10 * time.Second
	radarVelocityMaxAge      = 30 * time.Minute

	// v2.0.84 radar auto-close gates. Every constant comes from the
	// 2026-09-03T20:35Z..09-04T19:29Z REAL backtest (24 band>=3 episodes,
	// 13 bots) that priced the policy — see migration 0042:
	//   radarAutocloseCooldown 1h/bot: the re-center cooldown is dist-aware
	//     (15m..2h) but a close is terminal — the window only guards
	//     event-spam when RequestBotClose loses the race to a stop;
	//   radarAutocloseStrictDistATR 0.5: the saved trades fired at dist
	//     0.04-0.45 (SNXXX 0.06, APT 0.35, ICP 0.41, XMR 0.44) while the
	//     biggest false-positive class (NEAR #670 recovering to +1.48) sat
	//     at 0.62-0.78;
	//   dwell 3 / age 30m reuse the re-center gates (radarDwellTicks,
	//     radarMinBotAge) — the signal must persist and the bot must have a
	//     history worth trusting.
	radarAutocloseCooldown      = 1 * time.Hour
	radarAutocloseStrictDistATR = 0.5
)

// adjustShiftProfitFloor is the local preference floor for the NORMAL
// adjust_params semantics (re-base with PnL realization). The exchange gate
// is the order's PURE FLOATING PnL > 0 (PROFIT_LESS_THAN_ZERO; prod NEAR #688
// 2026-09-04 19:05Z: grid profit +2 with floating −0.5 cleared the old
// total-based preflight and the shift was refused anyway), so the preflight
// watches the same leg — the floating (unrealized) figure from the last
// reconcile — and the +$0.10 buffer absorbs drift between the reconcile and
// the shift attempt. The realized grid profit never enters the decision.
var adjustShiftProfitFloor = decimal.NewFromFloat(0.10)

// Shift modes for a native range shift (v2.0.85 "shift always").
const (
	// shiftModeNormal is the legacy semantics: floating ≥ +$0.10, the
	// exchange re-bases the grid (base/investment recalculated, floating PnL
	// crystallizes into realized).
	shiftModeNormal = "normal"
	// shiftModeKeepInvestment is the documented keepInvestment=true rescue
	// transfer: pure range move, the exchange skips its
	// PROFIT_LESS_THAN_ZERO gate, the investment base and the local
	// targets/stops arithmetic stay untouched. There is no longer a locally
	// blocked state — an under-water bot is SHIFTED, not parked.
	shiftModeKeepInvestment = "keep_investment"
)

// adjustShiftMode resolves HOW a native range shift must be shipped from the
// last-reconciled FLOATING PnL: green (≥ +$0.10 buffer) → normal re-base;
// anything below → keep_investment transfer. It is the single preflight both
// the radar arms and the manage RANGE_BREAK path consult; the figure under
// test is the FLOATING PnL (unrealized leg of the last reconcile), not the
// bot total.
func adjustShiftMode(floatingPnL decimal.Decimal) string {
	if floatingPnL.GreaterThanOrEqual(adjustShiftProfitFloor) {
		return shiftModeNormal
	}
	return shiftModeKeepInvestment
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

// radarPricePoint is one (ts, price) sample of the in-memory velocity trail.
type radarPricePoint struct {
	at    time.Time
	price decimal.Decimal
}

// radarEdgeSpeedATRPer15m measures the price's speed TOWARD the adverse edge
// in ATR(15m) units per 15 minutes from two consecutive trail samples:
// long inventory falls toward its lower bound, short inventory rises toward
// its upper. Moving away from the edge reads 0 — the trigger is directional.
// atrPctEntry is the deploy-time ATR in % per 15m bar (radarInput.atrEntryPct);
// without it (0/unknown) there is no ruler and the speed is unmeasurable.
func radarEdgeSpeedATRPer15m(prev, cur radarPricePoint, atrPctEntry, inventorySide float64) float64 {
	if inventorySide == 0 || atrPctEntry <= 0 {
		return 0
	}
	if !prev.price.GreaterThan(decimal.Zero) || !cur.price.GreaterThan(decimal.Zero) {
		return 0
	}
	minutes := cur.at.Sub(prev.at).Minutes()
	if minutes <= 0 {
		return 0
	}
	var edgeMove float64
	if inventorySide > 0 {
		edgeMove, _ = prev.price.Sub(cur.price).Float64() // falling = toward the lower bound
	} else {
		edgeMove, _ = cur.price.Sub(prev.price).Float64() // rising = toward the upper bound
	}
	if edgeMove <= 0 {
		return 0
	}
	curPrice, _ := cur.price.Float64()
	atr15 := curPrice * atrPctEntry / 100.0
	if atr15 <= 0 {
		return 0
	}
	return (edgeMove / minutes) * 15.0 / atr15
}

// rememberRadarPrice advances the in-memory symbol→(ts,price) trail and
// returns the PREVIOUS point when it is a usable velocity baseline. The
// trail is deliberately not durable and not a cooldown — it is a pure
// measurement over two consecutive radar passes, so losing it on a restart
// costs one pass of velocity sensitivity, nothing else. Same-pass repeats
// (two bots on one symbol) and stale baselines (worker downtime) are not
// measurements: the stored point is kept/refreshed but reported unusable.
func (worker *Worker) rememberRadarPrice(symbol string, point radarPricePoint) (radarPricePoint, bool) {
	if worker.radarPriceTrail == nil {
		worker.radarPriceTrail = make(map[string]radarPricePoint)
	}
	prev, ok := worker.radarPriceTrail[symbol]
	if ok && point.at.Sub(prev.at) < radarVelocityFreshWindow {
		// The trail already carries this pass's price; keep it as baseline.
		return radarPricePoint{}, false
	}
	worker.radarPriceTrail[symbol] = point
	if !ok || prev.at.IsZero() || point.at.Sub(prev.at) > radarVelocityMaxAge ||
		!prev.price.GreaterThan(decimal.Zero) {
		return radarPricePoint{}, false
	}
	return prev, true
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

// radarMaybeRecenter executes the B2-early/B2-velocity/B3/B4 action matrix for
// one bot.
func (worker *Worker) radarMaybeRecenter(ctx context.Context, settings Settings, b radarInput, rs radarScores) {
	// v2.0.85 velocity trail FIRST — before every gate — so two consecutive
	// passes always leave a (ts,price) pair regardless of how early the bot
	// bails out (band < 2 today, band 2 tomorrow: the baseline must exist by
	// then, and losing one pass of history on restart is acceptable).
	now := time.Now().UTC()
	prevPoint, hasPrev := worker.rememberRadarPrice(b.symbol, radarPricePoint{at: now, price: b.price})
	velocitySpeed := 0.0
	if hasPrev {
		velocitySpeed = radarEdgeSpeedATRPer15m(prevPoint, radarPricePoint{at: now, price: b.price},
			b.atrEntryPct, b.inventorySide)
	}

	if settings.StopForecastMode != "ACTIVE" || rs.Band < 2 {
		return
	}

	// v2.0.76 "shift on green": band 2 arms the EARLY preventive re-center —
	// but only when the price has already covered 70%+ of the way to the
	// edge the inventory fears. B3 typically fires with the price at the stop
	// and the bot under water; the B2 window is where the re-center is still
	// cheap. v2.0.85 FIX-2 adds the VELOCITY lane inside the same band 2:
	// from 55% of the way to the edge, a price racing toward it at
	// ≥ 0.6×ATR(15m)/15m fires after ONE tick — the dwell-3 window slips past
	// on exactly those pairs. Velocity requires a green base (prevention,
	// not rescue); under water the ≥70% early lane now shifts anyway through
	// keepInvestment (FIX-1).
	early := rs.Band == 2
	velocity := false
	if early {
		progress := radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide)
		if hasPrev && progress >= radarB2VelocityEdgeProgressMin &&
			velocitySpeed >= radarB2VelocitySpeedATR15 {
			velocity = true
		} else if progress < radarB2EarlyEdgeProgressMin {
			return // too far from the edge for any band-2 lane
		}
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
	// B3/B4 over B3) — a single spike is noise. The velocity lane is the
	// documented exception (dwell 1): speed is itself the persistence proof,
	// waiting 3 ticks is how fast pairs escape the window. Checked after the
	// cooldown so the snapshot query only runs for cooldown-eligible bots.
	dwellThreshold := bandB3
	dwellTicks := radarDwellTicks
	if early {
		dwellThreshold = bandB2
	}
	if velocity {
		dwellTicks = 1
	}
	if worker.radarDwellAtOrAbove(ctx, b.botID, dwellThreshold) < dwellTicks {
		return
	}

	// v2.0.72: REAL bots join the action matrix. Shared gates above (mode,
	// band, durable cooldown, dwell) already ran; the arm differs only in
	// execution — native adjust_params instead of a simulated range UPDATE.
	if b.botSource == "REAL" {
		worker.radarRecenterReal(ctx, settings, b, rs, early, velocity, velocitySpeed)
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

	// v2.0.85 mode preflight, paper arm: the simulated shift must mirror the
	// semantics the exchange applies on REAL, or paper calibration keeps
	// endorsing maneuvers the native fleet executes differently. The FLOATING
	// leg (v2.0.83, the NEAR #688 lesson) selects the mode: green → normal
	// re-base with mark crystallization; under water → keep_investment
	// transfer (bounds move, the investment base / entry / PnL legs stay).
	// The velocity lane is prevention only and requires the normal mode.
	mode := adjustShiftMode(bot.unrealized)
	if velocity && mode != shiftModeNormal {
		return
	}

	newLower, newUpper := recenterBounds(bot.lower, bot.upper, b.price)
	newLevel := gridLevelForPrice(newLower, newUpper, bot.gridNum, b.price)

	// keep_investment semantics: a pure range transfer — nothing re-bases,
	// nothing crystallizes. quote_investment, entry, realized/unrealized and
	// the mark all stay exactly as they were (the native twin keeps its
	// investment on the exchange by contract); only the bounds, the grid
	// level and the anti-hunt travel.
	if mode == shiftModeKeepInvestment {
		setClauses := ""
		var extraArgs []any
		if bot.antiHunt != nil && bot.antiHunt.GreaterThan(decimal.Zero) {
			newStop := recenterStop(bot.direction, bot.lower, bot.upper, newLower, newUpper, *bot.antiHunt)
			setClauses += fmt.Sprintf(", anti_hunt_stop_price = $%d", 5+len(extraArgs))
			extraArgs = append(extraArgs, newStop)
		}
		if _, err := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET lower_price = $2, upper_price = $3,
			    adjustments_count = adjustments_count + 1,
			    last_grid_level = $4`+setClauses+`
			WHERE id = $1 AND status = 'RUNNING'
		`, append([]any{b.botID, newLower, newUpper, newLevel}, extraArgs...)...); err != nil {
			worker.logger.Warn("stop-radar: keep-investment recenter UPDATE failed",
				"component", "autogrid_worker", "symbol", b.symbol, "error", err)
			return
		}
		reason := radarRecenterReason(rs.Band, early, velocity)
		total := bot.realized.Add(bot.unrealized)
		worker.logger.Info("stop-radar recentered grid (keep investment)",
			"component", "autogrid_worker", "symbol", b.symbol,
			"band", rs.Band, "score", decimal.NewFromFloat(rs.Score).Round(4).String(),
			"lower", newLower.String(), "upper", newUpper.String())
		if err := LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "PAPER", b.symbol, "ADJUST_RANGE", &b.price, &total, map[string]any{
			"reason": reason, "action": "RADAR_RECENTER",
			"mode":    shiftModeKeepInvestment,
			"score":   decimal.NewFromFloat(rs.Score).Round(4).String(),
			"band":    rs.Band,
			"s1":      decimal.NewFromFloat(rs.S1).Round(3).String(),
			"s2":      decimal.NewFromFloat(rs.S2).Round(3).String(),
			"s3":      decimal.NewFromFloat(rs.S3).Round(3).String(),
			"s4":      decimal.NewFromFloat(rs.S4).Round(3).String(),
			"new_lower": newLower.String(), "new_upper": newUpper.String(),
			"floating_pnl": bot.unrealized.StringFixed(4),
		}); err != nil {
			worker.logger.Warn("stop-radar: keep-investment recenter event insert failed — durable cooldown not armed",
				"component", "autogrid_worker", "symbol", b.symbol, "error", err)
		}
		_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
			"bot_number": b.botNumber, "symbol": b.symbol,
			"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
			"reason": reason, "mode": shiftModeKeepInvestment,
			"score": decimal.NewFromFloat(rs.Score).Round(3).String(),
		})
		return
	}

	// Normal mode: re-base with crystallization.
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

	reason := radarRecenterReason(rs.Band, early, velocity)
	total := bot.realized.Add(bot.unrealized)
	worker.logger.Info("stop-radar recentered grid",
		"component", "autogrid_worker", "symbol", b.symbol,
		"band", rs.Band, "score", decimal.NewFromFloat(rs.Score).Round(4).String(),
		"lower", newLower.String(), "upper", newUpper.String())

	// The event insert below ARMS the durable cooldown — a swallowed error
	// here would leave the next re-center un-gated on the following pass.
	if err := LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "PAPER", b.symbol, "ADJUST_RANGE", &b.price, &total, map[string]any{
		"reason": reason, "action": "RADAR_RECENTER",
		"mode":    shiftModeNormal,
		"score":   decimal.NewFromFloat(rs.Score).Round(4).String(),
		"band":    rs.Band,
		"s1":      decimal.NewFromFloat(rs.S1).Round(3).String(),
		"s2":      decimal.NewFromFloat(rs.S2).Round(3).String(),
		"s3":      decimal.NewFromFloat(rs.S3).Round(3).String(),
		"s4":      decimal.NewFromFloat(rs.S4).Round(3).String(),
		"new_lower": newLower.String(), "new_upper": newUpper.String(),
	}); err != nil {
		worker.logger.Warn("stop-radar: recenter event insert failed — durable cooldown not armed",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
	}
	if velocity {
		queueRadarB2VelocityTelegram(ctx, worker, b, rs, newLower, newUpper, velocitySpeed)
		return
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

// radarRecenterReason picks the durable ADJUST_RANGE reason: the velocity lane
// is the most specific, then the early lane, then the B4 escape, then B3.
func radarRecenterReason(band int, early, velocity bool) string {
	switch {
	case velocity:
		return "RADAR_B2_VELOCITY_RECENTER"
	case early:
		return "RADAR_B2_EARLY_RECENTER"
	case band >= 4:
		return "RADAR_B4_ESCAPE"
	default:
		return "RADAR_B3_RECENTER"
	}
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
		"score":             decimal.NewFromFloat(rs.Score).Round(3).String(),
		"edge_progress_pct": decimal.NewFromFloat(progress * 100).Round(0).String(),
		"total":             b.total.StringFixed(2),
	})
}

// queueRadarB2VelocityTelegram renders the velocity-lane notification: the
// one-tick trajectory catch is a third, faster class of preventive shift and
// must be distinguishable from both the dwell-3 early re-center and the
// B3/B4 escapes in the operator's feed.
func queueRadarB2VelocityTelegram(ctx context.Context, worker *Worker, b radarInput, rs radarScores, newLower, newUpper decimal.Decimal, speedATR15 float64) {
	progress := radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide)
	_ = QueueTelegramEvent(ctx, worker.db, "RADAR_B2_VELOCITY_RECENTER", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol,
		"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
		"score":             decimal.NewFromFloat(rs.Score).Round(3).String(),
		"edge_progress_pct": decimal.NewFromFloat(progress * 100).Round(0).String(),
		"speed_atr_15m":     decimal.NewFromFloat(speedATR15).Round(2).String(),
		"total":             b.total.StringFixed(2),
	})
}

// radarRecenterReal is the REAL arm of the radar action matrix (v2.0.72):
// the same B2-early/B2-velocity/B3/B4 geometry as paper (recenterBounds),
// executed through Service.AdjustBot mode=adjust_params — the identical path
// the manage loop's tranche/range shifts and the operator console use, so the
// exchange-side contract (live openPrice, row = existing grid_num,
// status=RUNNING + settings guard) cannot drift between callers. v2.0.85:
// an under-water bot ships keepInvestment=true (pure range transfer, no PnL
// realization, dry-run adjustParamsCheck first inside AdjustBot).
//
// Budget sharing with the manage path is structural, not duplicated: both
// read and increment the same grid_bots.adjustments_count, so a manage
// RANGE_BREAK shift and a radar re-center draw from one per-bot budget;
// the durable cooldown below arms on bot_execution_events by bot_id, where
// REAL ids already flow.
func (worker *Worker) radarRecenterReal(ctx context.Context, settings Settings, b radarInput, rs radarScores, early, velocity bool, velocitySpeed float64) {
	type realBot struct {
		direction    string
		lower, upper decimal.Decimal
		gridNum      int
		antiHunt     *decimal.Decimal
		adjustments  int
		floating     decimal.Decimal
		total        decimal.Decimal
		createdAt    time.Time
	}
	var bot realBot
	err := worker.db.QueryRow(ctx, `
		SELECT direction, lower_price, upper_price, grid_num,
		       NULLIF(anti_hunt_stop_price, 0),
		       COALESCE(adjustments_count, 0), created_at,
		       COALESCE(unrealized_pnl_usdt, 0),
		       COALESCE(realized_pnl_usdt, 0) + COALESCE(unrealized_pnl_usdt, 0)
		FROM grid_bots
		WHERE id = $1 AND autogrid_settings_id = $2
		  AND bu_order_id IS NOT NULL AND status = 'RUNNING'
	`, b.botID, settings.ID).Scan(
		&bot.direction, &bot.lower, &bot.upper, &bot.gridNum,
		&bot.antiHunt, &bot.adjustments, &bot.createdAt,
		&bot.floating, &bot.total,
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

	// v2.0.85 mode preflight (the v2.0.83 floating-gate lesson stays): the
	// exchange's normal adjust_params semantics require the order's PURE
	// FLOATING PnL > 0 (BOT_INVALID_ARGUMENT/PROFIT_LESS_THAN_ZERO; prod NEAR
	// #688: total +1.5 with floating −0.5 refused at 19:05Z 09-04). The mode
	// therefore keys on the floating leg of the last reconcile: green →
	// normal re-base; under water → keepInvestment rescue transfer (FIX-1),
	// dry-run validated by adjustParamsCheck inside AdjustBot before the
	// live call. No bot is parked anymore — if the exchange refuses even the
	// keep transfer, the RADAR_RECENTER_FAILED path below carries the reason.
	// The velocity lane is prevention only and requires the normal mode.
	mode := adjustShiftMode(bot.floating)
	if velocity && mode != shiftModeNormal {
		return
	}
	keepInvestment := mode == shiftModeKeepInvestment

	// AdjustBot increments adjustments_count only AFTER the native
	// adjustParams succeeds — a failed adjust (or a refused adjustParamsCheck
	// dry-run) returns here with the budget and geometry untouched (Warn,
	// never a phantom shift).
	if _, err := worker.service.AdjustBot(ctx, worker.accounts, settings.ID, b.botID, AdjustBotInput{
		Mode:           "adjust_params",
		Lower:          newLower,
		Upper:          newUpper,
		Row:            bot.gridNum,
		KeepInvestment: &keepInvestment,
	}); err != nil {
		worker.logger.Warn("stop-radar: REAL recenter adjust_params failed — budget untouched",
			"component", "autogrid_worker", "symbol", b.symbol, "mode", mode, "error", err)
		// v2.0.75 deaf-branch fix: the exchange rejecting adjustParams used
		// to leave ONLY this Warn — no event, no telegram, no backoff, so the
		// radar retried invisibly forever while the operator saw "радар
		// молчал" (prod AXTIX #668: band-4 with satisfied dwell for 8h, zero
		// actions, zero traces). One durable RADAR_RECENTER_FAILED event per
		// hour (model_state marker, the tranche2SkipAt pattern) + a telegram
		// line — the failure becomes a first-class signal. v2.0.85: this is
		// also the ONLY remaining refusal path — a keepInvestment transfer
		// the exchange rejects in adjustParamsCheck lands here with the
		// check's reason in the error.
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
					"mode":           mode,
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

	reason := radarRecenterReason(rs.Band, early, velocity)
	worker.logger.Info("stop-radar recentered REAL grid",
		"component", "autogrid_worker", "symbol", b.symbol,
		"band", rs.Band, "score", decimal.NewFromFloat(rs.Score).Round(4).String(),
		"mode", mode,
		"lower", newLower.String(), "upper", newUpper.String())

	// The event insert below ARMS the durable cooldown — a swallowed error
	// here would leave the next re-center un-gated on the following pass.
	// The mode detail (normal|keep_investment) records WHICH exchange
	// semantics the shift used; after a keep_investment transfer the local
	// quote_investment/targets/stops arithmetic deliberately stays on the old
	// base (nothing re-based), while the bounds and the anti-hunt moved.
	if err := LogBotEvent(ctx, worker.db, b.botID, b.botNumber, "REAL", b.symbol, "ADJUST_RANGE", &b.price, &bot.total, map[string]any{
		"reason": reason, "action": "RADAR_RECENTER",
		"mode":    mode,
		"score":   decimal.NewFromFloat(rs.Score).Round(4).String(),
		"band":    rs.Band,
		"s1":      decimal.NewFromFloat(rs.S1).Round(3).String(),
		"s2":      decimal.NewFromFloat(rs.S2).Round(3).String(),
		"s3":      decimal.NewFromFloat(rs.S3).Round(3).String(),
		"s4":      decimal.NewFromFloat(rs.S4).Round(3).String(),
		"new_lower": newLower.String(), "new_upper": newUpper.String(),
		"floating_pnl": bot.floating.StringFixed(4),
	}); err != nil {
		worker.logger.Warn("stop-radar: REAL recenter event insert failed — durable cooldown not armed",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
	}
	if velocity {
		queueRadarB2VelocityTelegram(ctx, worker, b, rs, newLower, newUpper, velocitySpeed)
		return
	}
	if early {
		queueRadarB2EarlyTelegram(ctx, worker, b, rs, newLower, newUpper)
		return
	}
	_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol,
		"lower_price": newLower.StringFixed(6), "upper_price": newUpper.StringFixed(6),
		"reason": reason, "mode": mode,
		"score":             decimal.NewFromFloat(rs.Score).Round(3).String(),
		"adjustments_count": bot.adjustments + 1,
	})
}

// radarAutocloseLastAt is the durable auto-close cooldown source: the newest
// RADAR_AUTOCLOSE event for the bot (the 0035 pattern — restart-proof). The
// event is written BEFORE the close intent, so a close that loses the race to
// a native stop still arms the hour: the operator sees one decision, not one
// per pass.
func (worker *Worker) radarAutocloseLastAt(ctx context.Context, botID string) (time.Time, bool) {
	var at *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(created_at)
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_AUTOCLOSE'
	`, botID).Scan(&at); err != nil || at == nil {
		return time.Time{}, false
	}
	return *at, true
}

// radarMaybeAutoclose (v2.0.84) closes a bot the radar is convinced is going
// to its stop. Opt-in via autogrid_settings.radar_autoclose_mode:
//
//	OFF    default — the 2026-09-04 backtest put the entire BAND3 surplus on
//	       one trade (SNXXX #669 +$10.2 of +$16.4 saved), so nothing fires
//	       until the operator arms it;
//	BAND3  band>=3 + under water (floating<0) + dwell>=3 + age>30m + 1h
//	       per-bot cooldown;
//	STRICT the BAND3 gates plus dist_to_stop < 0.5 ATR-sigma, and only when
//	       the distance is actually known (S1>0 — a bot without an anchored
//	       anti-hunt stop has no meaningful dist and must never fire).
//
// Invariants:
//   - NEVER close a bot in the green: floating >= 0 returns in every mode
//     (prod counter-examples #651/#670/#675 flagged band3 in profit and all
//     survived to profit);
//   - shift priority, v2.0.85 form: under water is no longer the shift's
//     inverse — a keepInvestment rescue transfer is always executable, so
//     the re-center matrix runs FIRST (radarPass order) and the close must
//     respect the SAME durable re-center cooldown: a bot that was just
//     rescue-shifted is given its dist-aware window to prove the new range
//     before any close is considered;
//   - SHADOW stays observe-only: auto-close requires StopForecastMode ACTIVE,
//     the 0029 contract;
//   - parity: REAL and PAPER both exit through Service.RequestBotClose — the
//     REAL arm gets STOP_REQUESTED + reconcile-cancel, the paper arm settles
//     at the mark, one code path.
//
// The event is logged BEFORE the close so the ledger explains the decision
// even when the close itself loses the race (the event also arms the 1h
// cooldown — a swallowed close must not re-fire every pass).
func (worker *Worker) radarMaybeAutoclose(ctx context.Context, settings Settings, b radarInput, rs radarScores) {
	mode := settings.RadarAutoCloseMode
	if mode != "BAND3" && mode != "STRICT" {
		return // OFF / blank: the shipping default, operator opt-in only
	}
	if settings.StopForecastMode != "ACTIVE" {
		return // SHADOW computes and warns, never touches the exit ladder
	}
	if rs.Band < 3 {
		return
	}

	// Per-bot facts from the source of truth (status RUNNING filtered in the
	// query itself — a close that won the race must not re-fire).
	var floating, total decimal.Decimal
	var createdAt time.Time
	if b.botSource == "REAL" {
		err := worker.db.QueryRow(ctx, `
			SELECT COALESCE(unrealized_pnl_usdt, 0),
			       COALESCE(realized_pnl_usdt, 0) + COALESCE(unrealized_pnl_usdt, 0),
			       created_at
			FROM grid_bots
			WHERE id = $1 AND autogrid_settings_id = $2
			  AND bu_order_id IS NOT NULL AND status = 'RUNNING'
		`, b.botID, settings.ID).Scan(&floating, &total, &createdAt)
		if err != nil {
			return
		}
	} else {
		err := worker.db.QueryRow(ctx, `
			SELECT COALESCE(unrealized_pnl_usdt, 0),
			       COALESCE(realized_pnl_usdt, 0) + COALESCE(unrealized_pnl_usdt, 0),
			       opened_at
			FROM paper_grid_bots
			WHERE id = $1 AND settings_id = $2 AND status = 'RUNNING'
		`, b.botID, settings.ID).Scan(&floating, &total, &createdAt)
		if err != nil {
			return
		}
	}

	// The never-close-green invariant, every mode: the under-water leg is the
	// FLOATING PnL — the same leg the exchange preflight watches. A bot with
	// banked grid profit and a sinking position is exactly the rescue class
	// the backtest says to keep (FARTCOIN #679: flagged under water at $0,
	// closed green +$1.10 two hours later).
	if !floating.IsNegative() {
		return
	}
	// Same trust gate as the re-center arm: a bot younger than 30 minutes has
	// no radar history worth trusting.
	if time.Since(createdAt) < radarMinBotAge {
		return
	}
	// Dwell: the band-3 signal must have persisted (shared gate with the
	// re-center matrix — the flicker-tolerant counter over the last
	// radarDwellTicks+1 snapshots).
	if worker.radarDwellAtOrAbove(ctx, b.botID, bandB3) < radarDwellTicks {
		return
	}
	// STRICT adds the proximity gate: only fire with a KNOWN distance under
	// 0.5 ATR-sigma. S1<=0 means no anchored stop → dist_to_stop_atr is a
	// default zero, not a measured "at the barrier" — never fire on it.
	if mode == "STRICT" && (rs.S1 <= 0 || rs.DistToStopATR >= radarAutocloseStrictDistATR) {
		return
	}
	// Durable 1h per-bot cooldown (the RADAR_AUTOCLOSE event below arms it).
	if lastAt, ok := worker.radarAutocloseLastAt(ctx, b.botID); ok &&
		time.Since(lastAt) < radarAutocloseCooldown {
		return
	}
	// Shift priority, v2.0.85: a rescue re-center (possibly keepInvestment,
	// possibly only minutes ago) owns the bot for its dist-aware window —
	// closing inside it would burn the shift we just paid for.
	if lastAt, ok := worker.radarLastActionAt(ctx, b.botID); ok &&
		time.Since(lastAt) < radarActionCooldownFor(rs.DistToStopATR) {
		return
	}

	reason := "RADAR_AUTOCLOSE_" + mode
	// Event + telegram BEFORE the close: the ledger explains the decision
	// even if the close itself loses the race to a native stop.
	_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
		"RADAR_AUTOCLOSE", &b.price, &total, map[string]any{
			"mode": mode, "reason": reason,
			"band": rs.Band, "score": decimal.NewFromFloat(rs.Score).Round(4).String(),
			"dist_to_stop_atr": decimal.NewFromFloat(rs.DistToStopATR).Round(4).String(),
			"floating_pnl":     floating.StringFixed(4),
			"total_pnl":        total.StringFixed(4),
		})
	_ = QueueTelegramEvent(ctx, worker.db, "RADAR_AUTOCLOSE", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol, "band": rs.Band,
		"score": decimal.NewFromFloat(rs.Score).Round(3).String(),
		"total": total.StringFixed(2), "mode": mode, "reason": reason,
	})

	if _, err := worker.service.RequestBotClose(ctx, settings.ID, b.botID, reason); err != nil {
		worker.logger.Warn("stop-radar: autoclose request failed — cooldown armed by the event",
			"component", "autogrid_worker", "symbol", b.symbol, "error", err)
		return
	}
	worker.logger.Info("stop-radar auto-closed bot",
		"component", "autogrid_worker", "symbol", b.symbol,
		"mode", mode, "band", rs.Band,
		"score", decimal.NewFromFloat(rs.Score).Round(4).String(),
		"total", total.StringFixed(4))
}
