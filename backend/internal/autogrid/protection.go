package autogrid

import (
	"context"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// v2.0.111 protective layer — four levers, each born from a paid prod
// incident on 2026-09-26:
//
// A. BREAK-FLIP (ORDI #1396 −$10.45): a NEUTRAL grid at band≥3, under water,
//    with the price ≥85% of the way to the adverse edge AND racing toward it
//    re-centers keep_investment today — dragging the toxic inventory into
//    the shifted range where a continued move compounds it. When the adverse
//    momentum is strong the escape lane now CLOSES the bot as a range break
//    (RANGE_BREAK_UP/DOWN) and queues the existing DGT re-deploy: a fresh
//    grid centered at the breakout with the same capital and NO legacy bag.
//
// B. TRANCHE STRESS MORATORIUM (the same ORDI: tranche doubled $50→$100 at
//    band 1, 44 minutes before the escape): no second tranche while the
//    bot's radar band is ≥2 or a fleet storm is active — adding margin into
//    stress doubles exactly the position the radar is trying to save.
//
// C. STORM MODE (ICP #1381 −$4.34, rotated into the bottom of a fleet-wide
//    alt flush): when ≥3 fleet symbols trip the realtime sharp-move trigger
//    within 5 minutes, the fleet enters a 30-minute rolling storm —
//    half-life rotations and new deploys defer (crystallizing a loss into
//    an acceleration is the most expensive timing there is), existing stops
//    and the radar keep working untouched.
//
// D. NATIVE LOSS-STOP (SUI −$4.84, ORDI overshoot +31% over cap): REAL
//    deploys now carry the exchange-side lossStop at −maxLoss, so the fill
//    is bounded by Pionex's own executor even if our whole process is down.

const (
	// Break-flip gate: adverse-edge progress and velocity thresholds.
	breakFlipEdgeProgressMin = 0.85
	breakFlipSpeedATR15      = radarB2VelocitySpeedATR15 // 0.6×ATR/15m
	breakFlipMinFloat        = -1.0                      // only escape real bags, not noise

	// Storm mode: ≥3 fleet symbols tripping within the window arms a
	// rolling storm; each new trip refreshes it.
	stormSymbolWindow = 5 * time.Minute
	stormMinSymbols   = 3
	stormDuration     = 30 * time.Minute
)

// ── A. break-flip ─────────────────────────────────────────────────────────

// radarBreakFlip checks the escape-flip conditions and, when met, closes the
// REAL NEUTRAL bot as a range break with a queued DGT re-deploy. Returns
// true when the flip path took over (the caller must not re-center).
func (worker *Worker) radarBreakFlip(ctx context.Context, settings Settings, b radarInput, rs radarScores, velocitySpeed float64) bool {
	if b.botSource != "REAL" || rs.Band < 3 {
		return false
	}
	total := b.total
	if total.GreaterThan(decimal.NewFromFloat(breakFlipMinFloat)) {
		return false // not underwater enough to pay the escape cost
	}
	progress := radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide)
	if progress < breakFlipEdgeProgressMin || velocitySpeed < breakFlipSpeedATR15 {
		return false
	}

	// The direction of the escape names the break: short inventory fleeing a
	// rise is an UP break, long inventory fleeing a fall is a DOWN break.
	breakReason := "RANGE_BREAK_DOWN"
	if b.inventorySide < 0 {
		breakReason = "RANGE_BREAK_UP"
	}

	var direction, accountID string
	var investment decimal.Decimal
	if err := worker.db.QueryRow(ctx, `
		SELECT direction, COALESCE(account_id, ''), quote_investment
		FROM grid_bots WHERE id = $1 AND status = 'RUNNING'
	`, b.botID).Scan(&direction, &accountID, &investment); err != nil {
		return false // lost the race with a close — let the caller proceed
	}
	if direction != "NEUTRAL" || accountID == "" {
		return false
	}

	tag, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED', closed_reason = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'RUNNING'
	`, b.botID, breakReason)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	worker.logger.Warn("radar break-flip: escape lane closes the grid for DGT re-deploy",
		"component", "autogrid_worker", "bot_number", b.botNumber, "symbol", b.symbol,
		"break_reason", breakReason, "total", total.StringFixed(2),
		"velocity_atr15", velocitySpeed, "edge_progress", progress)
	_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
		"RADAR_BREAK_FLIP", &b.price, &total, map[string]any{
			"break_reason": breakReason, "band": rs.Band,
			"velocity_atr15": decimal.NewFromFloat(velocitySpeed).Round(3).String(),
			"edge_progress":  decimal.NewFromFloat(progress).Round(3).String(),
		})
	_ = QueueTelegramEvent(ctx, worker.db, "RADAR_BREAK_FLIP", map[string]any{
		"bot_number": b.botNumber, "symbol": b.symbol, "break_reason": breakReason,
		"total":   total.StringFixed(2),
		"message": "пробой с ускорением — сетка закрыта, DGT перезапустит с центра пробоя без старого инвентаря",
	})

	// DGT intent onto the closing row: fires from the existing
	// processDgtRealRedeployIntents once the terminal settle lands.
	worker.dgtQueueRealRedeploy(ctx, settings, dgtRedeploySpec{
		symbol:       b.symbol,
		direction:    direction,
		slotBudget:   investment,
		oldBotID:     b.botID,
		oldBotNumber: b.botNumber,
		atrFallback:  b.atrEntryPct,
		accountID:    accountID,
		breakPrice:   b.price,
	})
	return true
}

// ── B+C shared: stress gates ──────────────────────────────────────────────

// trancheStressAllowed reports whether the tranche-2 lane may fire for the
// bot right now: no active storm and the bot's latest radar band below 2.
func (worker *Worker) trancheStressAllowed(ctx context.Context, botID string) bool {
	if worker.stormActive() {
		return false
	}
	if band, _, ok := worker.lastRadarSnapshot(ctx, botID); ok && band >= 2 {
		return false
	}
	return true
}

// ── C. storm mode ─────────────────────────────────────────────────────────

// noteStormTrigger records one realtime sharp-move trigger and arms/extends
// the storm when enough fleet symbols trip together. Called from the WS
// callback goroutine — everything here is mutex-guarded.
func (worker *Worker) noteStormTrigger(symbol string) {
	now := time.Now()
	worker.stormMu.Lock()
	defer worker.stormMu.Unlock()
	if worker.stormTriggers == nil {
		worker.stormTriggers = make(map[string]time.Time)
	}
	worker.stormTriggers[symbol] = now
	distinct := 0
	for sym, at := range worker.stormTriggers {
		if now.Sub(at) > stormSymbolWindow {
			delete(worker.stormTriggers, sym)
			continue
		}
		distinct++
	}
	if distinct >= stormMinSymbols {
		worker.stormUntil = now.Add(stormDuration)
	}
}

// stormActive reports whether the fleet storm window is open.
func (worker *Worker) stormActive() bool {
	worker.stormMu.RLock()
	defer worker.stormMu.RUnlock()
	return time.Now().Before(worker.stormUntil)
}

// stormState is the one-line status for logs and the console.
func (worker *Worker) stormState() (active bool, symbols int, until time.Time) {
	worker.stormMu.RLock()
	defer worker.stormMu.RUnlock()
	now := time.Now()
	active = now.Before(worker.stormUntil)
	symbols = 0
	for _, at := range worker.stormTriggers {
		if now.Sub(at) <= stormSymbolWindow {
			symbols++
		}
	}
	return active, symbols, worker.stormUntil
}

// maybeLogStormState logs arm/extend transitions at most once per arm.
// The state is computed INLINE under the held Lock — stormState() takes its
// own RLock and Go's RWMutex is not reentrant: the Lock→RLock nest deadlocked
// the whole supervision loop on the first pass (adversarial review,
// agent_3217d33f, the one finding that alone disqualified the tag).
func (worker *Worker) maybeLogStormState(ctx context.Context) {
	worker.stormMu.Lock()
	now := time.Now()
	active := now.Before(worker.stormUntil)
	symbols := 0
	for _, at := range worker.stormTriggers {
		if now.Sub(at) <= stormSymbolWindow {
			symbols++
		}
	}
	shouldNotify := active && now.Sub(worker.stormLoggedAt) > stormDuration/2
	if shouldNotify {
		worker.stormLoggedAt = now
	}
	until := worker.stormUntil
	worker.stormMu.Unlock()
	if !shouldNotify {
		return
	}
	if worker.db == nil {
		return
	}
	worker.logger.Warn("storm mode armed: rotations and deploys deferred",
		"component", "autogrid_worker", "symbols_tripped", symbols,
		"until", until.Format("15:04:05"))
	_ = QueueTelegramEvent(ctx, worker.db, "STORM_MODE", map[string]any{
		"message": "шторм-режим: ≥3 символов флота в ускорении — ротации и новые деплои заморожены до " +
			until.Format("15:04") + " UTC, стопы и радар работают",
		"symbols": symbols,
	})
}

// symbolOf extracts the bare base for logging helpers.
func symbolOf(symbol string) string {
	return strings.TrimSuffix(strings.TrimSuffix(symbol, "_PERP"), "_USDT_PERP")
}
