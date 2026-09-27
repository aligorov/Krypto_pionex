package autogrid

import (
	"context"
	"fmt"
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
	if b.botSource != "REAL" {
		return false
	}
	total := b.total
	if total.GreaterThan(decimal.NewFromFloat(breakFlipMinFloat)) {
		return false // not underwater enough to pay the escape cost
	}
	progress := radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide)
	isOFIConfirmed := b.ofiActionable && ((b.inventorySide > 0 && b.ofiRegime == "CONFIRMED_DUMP") ||
		(b.inventorySide < 0 && b.ofiRegime == "CONFIRMED_PUMP"))

	// Standard trigger: progress >= 0.85 and velocity >= breakFlipSpeedATR15 in band >= 3.
	// OFI accelerated escape: if institutional flow is confirmed adverse, trigger earlier at progress >= 0.60
	// to prevent absorbing the full knife.
	standardTrigger := rs.Band >= 3 && progress >= breakFlipEdgeProgressMin && velocitySpeed >= breakFlipSpeedATR15
	ofiTrigger := isOFIConfirmed && progress >= 0.60

	if !standardTrigger && !ofiTrigger {
		return false
	}

	escapeTrigger := "VELOCITY_BREAKOUT"
	if ofiTrigger {
		escapeTrigger = "OFI_CONFIRMED_" + b.ofiRegime
	}

	// The direction of the escape names the break: short inventory fleeing a
	// rise is an UP break, long inventory fleeing a fall is a DOWN break.
	breakReason := "RANGE_BREAK_DOWN"
	if b.inventorySide < 0 {
		breakReason = "RANGE_BREAK_UP"
	}

	var direction, accountID string
	var investment decimal.Decimal
	// v2.0.136: account_id is a NOT NULL uuid — the old COALESCE(id,'')
	// failed the uuid cast at plan time on EVERY call, so the swallowed
	// error kept the whole break-flip escape lane (and its DGT intent)
	// dead in production.
	if err := worker.db.QueryRow(ctx, `
		SELECT direction, account_id::TEXT, quote_investment
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
		"break_reason", breakReason, "escape_trigger", escapeTrigger, "ofi_regime", b.ofiRegime,
		"total", total.StringFixed(2),
		"velocity_atr15", velocitySpeed, "edge_progress", progress)
	_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
		"RADAR_BREAK_FLIP", &b.price, &total, map[string]any{
			"break_reason":   breakReason,
			"escape_trigger": escapeTrigger,
			"ofi_regime":     b.ofiRegime,
			"band":           rs.Band,
			"velocity_atr15": decimal.NewFromFloat(velocitySpeed).Round(3).String(),
			"edge_progress":  decimal.NewFromFloat(progress).Round(3).String(),
		})
	_ = QueueTelegramEvent(ctx, worker.db, "RADAR_BREAK_FLIP", map[string]any{
		"bot_number":     b.botNumber,
		"symbol":         b.symbol,
		"break_reason":   breakReason,
		"escape_trigger": escapeTrigger,
		"ofi_regime":     b.ofiRegime,
		"total":          total.StringFixed(2),
		"status":         "STOP_REQUESTED",
		"message":        "пробой (" + escapeTrigger + ") — запрошена остановка сетки (STOP_REQUESTED), DGT перезапустит с центра пробоя после закрытия",
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

// radarMicrostructureEmergencyExit executes a decoupled fast-path protective exit
// for bots suffering confirmed adverse institutional order flow (CONFIRMED_DUMP / CONFIRMED_PUMP).
// It bypasses the 90s radar score throttle, up to 2-hour recenter cooldowns, and 3-snapshot dwell gates.
//
// In SHADOW mode (settings.StopForecastMode != "ACTIVE"):
// - Strictly observational: emits STOP_FORECAST_SHADOW events and Telegram advisory,
//   arms 2m shadow debounce, and never touches bot state or trading execution.
//
// In ACTIVE mode (settings.StopForecastMode == "ACTIVE"):
// - REAL NEUTRAL bots with DGT re-deploy enabled: executes radarBreakFlip (closes grid & queues center-aligned re-deploy).
// - Directional bots (LONG / SHORT), REAL bots without DgtRedeploy, and PAPER bots:
//   executes an immediate direct emergency protective stop (EMERGENCY_OFI_DUMP or EMERGENCY_OFI_PUMP).
//   On DB write failure: arms 2s transient backoff.
//   On successful DB write: arms 30s debounce and records requested_at timestamp.
func (worker *Worker) radarMicrostructureEmergencyExit(ctx context.Context, settings Settings, b radarInput) bool {
	if !b.ofiActionable {
		return false
	}
	if !b.total.IsNegative() {
		return false
	}

	isAdverseDump := b.inventorySide > 0 && b.ofiRegime == "CONFIRMED_DUMP"
	isAdversePump := b.inventorySide < 0 && b.ofiRegime == "CONFIRMED_PUMP"
	if !isAdverseDump && !isAdversePump {
		return false
	}

	progress := radarB2EarlyEdgeProgress(b.price, b.lower, b.upper, b.inventorySide)
	if progress < 0.60 {
		return false
	}

	adverseReason := "EMERGENCY_OFI_DUMP"
	if isAdversePump {
		adverseReason = "EMERGENCY_OFI_PUMP"
	}

	// 1. SHADOW mode safety gate: strictly observational!
	// Never mutate bot status, never stop real positions, never close paper bots.
	// Uses dedicated emergencyShadowDebounce so switching to ACTIVE immediately executes without delay!
	if settings.StopForecastMode != "ACTIVE" {
		if worker.isEmergencyShadowDebounced(b.botID) {
			return false
		}
		worker.armEmergencyShadowDebounce(b.botID, 2*time.Minute)
		if worker.logger != nil {
			worker.logger.Info("microstructure emergency protective exit (SHADOW observation)",
				"component", "autogrid_worker", "bot_number", b.botNumber, "symbol", b.symbol,
				"direction", b.direction, "reason", adverseReason, "ofi_regime", b.ofiRegime,
				"total", b.total.StringFixed(2), "edge_progress", progress)
		}
		if worker.db != nil {
			_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
				"STOP_FORECAST_SHADOW", &b.price, &b.total, map[string]any{
					"trigger":       adverseReason,
					"ofi_regime":    b.ofiRegime,
					"direction":     b.direction,
					"edge_progress": decimal.NewFromFloat(progress).Round(3).String(),
					"total":         b.total.StringFixed(2),
					"advisory":      "emergency exit criteria met in SHADOW mode (no trade action)",
				})
			_ = QueueTelegramEvent(ctx, worker.db, "STOP_FORECAST_SHADOW", map[string]any{
				"bot_number":    b.botNumber,
				"symbol":        b.symbol,
				"direction":     b.direction,
				"reason":        adverseReason,
				"ofi_regime":    b.ofiRegime,
				"total":         b.total.StringFixed(2),
				"edge_progress": fmt.Sprintf("%.0f%%", progress*100),
				"message":       fmt.Sprintf("👀 [SHADOW] экстренная защитная остановка (%s, %s): институциональный поток против позиции, прогресс %.0f%% — в режиме ACTIVE сетка была бы остановлена", b.symbol, b.ofiRegime, progress*100),
			})
			// v2.0.138 journal: the SHADOW observation — verdict SKIP (the
			// criteria fired, execution is off). The feature vector comes
			// from the same in-memory lane the decision read.
			if worker.ofiEngine != nil {
				analysis := worker.ofiEngine.Analyze(b.symbol)
				logOFIDecision(ctx, worker.db, settings.ID, b.symbol, ofiKindEmergency,
					analysis, string(analysis.Readiness()), "SKIP",
					"shadow: критерий выполнен, исполнения нет (StopForecastMode != ACTIVE)",
					fmt.Sprintf("#%d", b.botNumber))
			}
		}
		return false
	}

	// 2. ACTIVE mode: Check execution debounce
	if worker.isEmergencyExitDebounced(b.botID) {
		return false
	}

	if worker.db == nil {
		worker.armEmergencyExitDebounce(b.botID, 2*time.Second)
		return false
	}

	// 3. ACTIVE: Emergency stop for REAL bots (NEUTRAL, LONG, SHORT)
	if b.botSource == "REAL" {
		now := time.Now().UTC()
		nowStr := now.Format(time.RFC3339)
		tag, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'STOP_REQUESTED', closed_reason = $2, updated_at = NOW()
			WHERE id = $1 AND status = 'RUNNING'
		`, b.botID, adverseReason)
		if err != nil || tag.RowsAffected() == 0 {
			worker.armEmergencyExitDebounce(b.botID, 2*time.Second)
			return false
		}

		worker.armEmergencyExitDebounce(b.botID, 30*time.Second)

		worker.logger.Warn("microstructure emergency protective exit requested (STOP_REQUESTED)",
			"component", "autogrid_worker", "bot_number", b.botNumber, "symbol", b.symbol,
			"direction", b.direction, "reason", adverseReason, "ofi_regime", b.ofiRegime,
			"total", b.total.StringFixed(2), "edge_progress", progress, "requested_at", nowStr)

		// v2.0.138 journal: the ACTIVE close decision — verdict FIRED.
		if worker.ofiEngine != nil {
			analysis := worker.ofiEngine.Analyze(b.symbol)
			logOFIDecision(ctx, worker.db, settings.ID, b.symbol, ofiKindEmergency,
				analysis, string(analysis.Readiness()), "FIRED", adverseReason,
				fmt.Sprintf("#%d", b.botNumber))
		}

		_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
			adverseReason, &b.price, &b.total, map[string]any{
				"closed_reason": adverseReason,
				"ofi_regime":    b.ofiRegime,
				"direction":     b.direction,
				"edge_progress": decimal.NewFromFloat(progress).Round(3).String(),
				"total":         b.total.StringFixed(2),
				"status":        "STOP_REQUESTED",
				"requested_at":  nowStr,
			})

		_ = QueueTelegramEvent(ctx, worker.db, adverseReason, map[string]any{
			"bot_number":    b.botNumber,
			"symbol":        b.symbol,
			"direction":     b.direction,
			"reason":        adverseReason,
			"ofi_regime":    b.ofiRegime,
			"total":         b.total.StringFixed(2),
			"edge_progress": fmt.Sprintf("%.0f%%", progress*100),
			"status":        "STOP_REQUESTED",
			"requested_at":  nowStr,
			"message":       fmt.Sprintf("🚨 запрошена экстренная защитная остановка (STOP_REQUESTED, %s): институциональный поток против позиции, прогресс к краю %.0f%% — ордера отменяются, позиция закрывается", b.ofiRegime, progress*100),
		})

		// DGT re-deploy intent for NEUTRAL bots (if enabled):
		// Decoupled from the emergency exit decision! The risk is ALREADY stopped above.
		// v2.0.136: account_id is a NOT NULL uuid — the old COALESCE(id,'')
		// failed uuid-cast at plan time and the swallowed error silently
		// dropped every REAL emergency DGT intent.
		if b.direction == "NEUTRAL" && settings.DgtRedeployEnabled {
			var accountID string
			var investment decimal.Decimal
			if err := worker.db.QueryRow(ctx, `
				SELECT account_id::TEXT, quote_investment
				FROM grid_bots WHERE id = $1
			`, b.botID).Scan(&accountID, &investment); err == nil && accountID != "" {
				worker.dgtQueueRealRedeploy(ctx, settings, dgtRedeploySpec{
					symbol:        b.symbol,
					direction:     b.direction,
					slotBudget:    investment,
					oldBotID:      b.botID,
					oldBotNumber:  b.botNumber,
					atrFallback:   b.atrEntryPct,
					accountID:     accountID,
					breakPrice:    b.price,
					emergencyExit: true,
				})
			}
		}

		return true
	}

	// 4. ACTIVE: PAPER bots
	if b.botSource == "PAPER" {
		now := time.Now().UTC()
		nowStr := now.Format(time.RFC3339)
		tag, err := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET status = 'COMPLETED', closed_reason = $2, mark_price = $3,
			    realized_pnl_usdt = $4, unrealized_pnl_usdt = 0,
			    closed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = 'RUNNING'
		`, b.botID, adverseReason, b.price, b.total)
		if err != nil || tag.RowsAffected() == 0 {
			worker.armEmergencyExitDebounce(b.botID, 2*time.Second)
			return false
		}

		worker.armEmergencyExitDebounce(b.botID, 30*time.Second)

		worker.logger.Warn("microstructure emergency protective exit (PAPER bot stopped)",
			"component", "autogrid_worker", "bot_number", b.botNumber, "symbol", b.symbol,
			"direction", b.direction, "reason", adverseReason, "ofi_regime", b.ofiRegime,
			"total", b.total.StringFixed(2), "edge_progress", progress)

		// v2.0.138 journal: the ACTIVE close decision — verdict FIRED (the
		// paper arm closes synchronously above).
		if worker.ofiEngine != nil {
			analysis := worker.ofiEngine.Analyze(b.symbol)
			logOFIDecision(ctx, worker.db, settings.ID, b.symbol, ofiKindEmergency,
				analysis, string(analysis.Readiness()), "FIRED", adverseReason,
				fmt.Sprintf("#%d", b.botNumber))
		}

		_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
			adverseReason, &b.price, &b.total, map[string]any{
				"closed_reason": adverseReason,
				"ofi_regime":    b.ofiRegime,
				"direction":     b.direction,
				"edge_progress": decimal.NewFromFloat(progress).Round(3).String(),
				"total":         b.total.StringFixed(2),
				"closed_at":     nowStr,
			})

		_ = QueueTelegramEvent(ctx, worker.db, adverseReason, map[string]any{
			"bot_number":    b.botNumber,
			"symbol":        b.symbol,
			"direction":     b.direction,
			"reason":        adverseReason,
			"ofi_regime":    b.ofiRegime,
			"total":         b.total.StringFixed(2),
			"edge_progress": fmt.Sprintf("%.0f%%", progress*100),
			"closed_at":     nowStr,
			"message":       fmt.Sprintf("экстренный защитный выход (PAPER, %s): институциональный поток против позиции — сетка закрыта", b.ofiRegime),
		})

		// DGT re-deploy for NEUTRAL PAPER bots (if enabled):
		if b.direction == "NEUTRAL" && settings.DgtRedeployEnabled {
			var investment decimal.Decimal
			var trancheBase *string
			var atrEntry float64
			var candidateID *string
			if err := worker.db.QueryRow(ctx, `
				SELECT quote_investment, NULLIF(model_state->>'trancheBase', ''),
				       COALESCE(NULLIF(model_state->>'atrPctEntry', '')::FLOAT8, 0),
				       candidate_id
				FROM paper_grid_bots WHERE id = $1
			`, b.botID).Scan(&investment, &trancheBase, &atrEntry, &candidateID); err == nil {
				worker.dgtRedeployPaper(ctx, settings, dgtRedeploySpec{
					symbol:        b.symbol,
					direction:     b.direction,
					breakPrice:    b.price,
					slotBudget:    slotCapital(trancheBase, investment),
					oldBotID:      b.botID,
					oldBotNumber:  b.botNumber,
					candidateID:   candidateID,
					atrFallback:   atrEntry,
					emergencyExit: true,
				})
			}
		}

		return true
	}

	return false
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
