package autogrid

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// v2.0.102 event-driven supervision: the 45s manage cadence let violent
// half-pass moves overshoot protective caps (SUI #1284: −$4.47 against the
// $4.00 cap). The WS lane already sees every mark move within ~1s; this
// layer turns a sharp move on a fleet symbol into an IMMEDIATE out-of-band
// manage pass — same gates, same actions, just not waiting for the ticker.
//
// Trigger rule: |ws − baseline| / baseline ≥ max(floorPct, k×ATR15m%),
// where baseline is the price the last completed pass sampled for that
// symbol and ATR15m is the bot's deploy-time ATR (model_state atrPctEntry).
// Debounced to one event pass per minEventPassSpacing so a runaway tape
// cannot DoS our own REST budget; a pass already in flight coalesces.
const (
	// Absolute trigger floor: below this the move is noise for any fleet
	// symbol (fee-gate geometry starts at 0.35% steps — half a step is a
	// sane "sharp" bar for symbols with no ATR on record).
	realtimeTriggerFloorPct = 0.30
	// ATR-scaled trigger: 60% of one 15m ATR between passes is a genuine
	// impulse, not drift.
	realtimeTriggerATRMult = 0.6
	// Minimum spacing between event-triggered passes. The base ticker keeps
	// its own cadence regardless.
	minEventPassSpacing = 10 * time.Second
)

// realtimePoint is the per-symbol baseline the last completed manage pass
// left behind (single manage goroutine owns the map; the WS callback only
// reads it under realtimeMu).
type realtimePoint struct {
	price  decimal.Decimal
	atrPct float64
}

// shouldTriggerRealtimePass is the pure trigger predicate.
func shouldTriggerRealtimePass(baseline, now decimal.Decimal, atrPct float64) bool {
	if !baseline.IsPositive() || !now.IsPositive() {
		return false
	}
	movePct, _ := now.Sub(baseline).Div(baseline).Mul(decimal.NewFromInt(100)).Abs().Float64()
	threshold := realtimeTriggerFloorPct
	if atrPct > 0 {
		if scaled := realtimeTriggerATRMult * atrPct; scaled > threshold {
			threshold = scaled
		}
	}
	return movePct >= threshold
}

// onRealtimeMark is the WS-lane callback: drop-out-safe (buffered-1 channel)
// and lock-light (RLock on the watch map only).
func (worker *Worker) onRealtimeMark(update pionex.MarkUpdate) {
	worker.realtimeMu.RLock()
	baseline, ok := worker.realtimeWatch[update.Symbol]
	worker.realtimeMu.RUnlock()
	if !ok {
		return
	}
	if !shouldTriggerRealtimePass(baseline.price, update.MarkPrice, baseline.atrPct) {
		return
	}
	select {
	case worker.realtimeSignal <- update.Symbol:
	default: // a signal is already queued — the pass will re-read marks anyway
	}
}

// handleRealtimeSignal runs one out-of-band manage pass, debounced. Called
// from the worker's main select loop, so all supervision stays
// single-goroutine — no new races against the manage loop's state.
func (worker *Worker) handleRealtimeSignal(symbol string, reconcileTicker interface{ Reset(time.Duration) }) {
	if !worker.lastEventPass.IsZero() && time.Since(worker.lastEventPass) < minEventPassSpacing {
		return
	}
	worker.lastEventPass = time.Now()
	worker.logger.Info("realtime trigger: sharp move — out-of-band manage pass",
		"component", "autogrid_worker", "symbol", symbol)
	worker.runGuarded("realtime_pass", func() {
		interval, err := worker.reconcileAndManage(context.Background())
		if err != nil {
			worker.logger.Error("realtime manage pass failed", "component", "autogrid_worker", "error", err)
		}
		if seconds := interval; seconds >= 15 && seconds <= 3600 {
			reconcileTicker.Reset(time.Duration(seconds) * time.Second)
		}
	})
}

// rememberRealtimeBaselines snapshots the per-symbol price/ATR baseline at
// the end of a completed manage pass. atrBySymbol comes from the fleet's
// deploy-time ATR readings (radar inputs); missing entries keep the old ATR
// or default to the absolute floor.
func (worker *Worker) rememberRealtimeBaselines(prices map[string]decimal.Decimal, atrBySymbol map[string]float64) {
	next := make(map[string]realtimePoint, len(prices))
	for sym, price := range prices {
		if !price.IsPositive() {
			continue
		}
		atr, ok := atrBySymbol[sym]
		if !ok {
			worker.realtimeMu.RLock()
			if prev, had := worker.realtimeWatch[sym]; had {
				atr = prev.atrPct
			}
			worker.realtimeMu.RUnlock()
		}
		next[sym] = realtimePoint{price: price, atrPct: atr}
	}
	// Only fleet-relevant symbols need watching; supersets cost nothing but
	// memory, so the full price map is fine.
	worker.realtimeMu.Lock()
	worker.realtimeWatch = next
	worker.realtimeMu.Unlock()
}
