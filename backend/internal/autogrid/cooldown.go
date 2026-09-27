package autogrid

import (
	"context"
)

// cooldownHours escalates the per-symbol protective-close cooldown window
// (v2.0.28). The flat 2h window let a pair stop out, wait 2 hours, re-enter
// the same trend signal near the channel top and stop AGAIN — double stops
// on VIRTUAL/NEAR (2026-08-20 night) and the CRWVX morning loop. Every
// additional protective close in the trailing 24h doubles the window:
// 1 close → 2h, 2 → 4h, 3 → 8h, 4 → 16h, 5+ → 24h saturation. A tape that
// keeps killing bots must stay closed longer than one that died once.
func cooldownHours(protectiveCloses int) int {
	if protectiveCloses <= 1 {
		return 2
	}
	hours := 2 << (protectiveCloses - 1) // 2 → 4 → 8 → 16
	if hours > 24 {
		return 24
	}
	return hours
}

// flipFamilyReasons are the protective-close reasons whose settled intents
// the v2.0.140 cascade-short flip can consume (package E).
const flipFamilyReasons = `'EMERGENCY_OFI_DUMP', 'RANGE_BREAK_DOWN'`

// cascadeFlipCooldownExempt (v2.0.140, review P1-1) reports whether the
// symbol's protective cooldown is armed SOLELY by the flip family AND the
// flip actually fired for the symbol in the trailing 24h — the only case in
// which the cascade-short lane may bypass the cooldown. The SHORT is the
// designed harvest of exactly the tape that closed the NEUTRAL; the flip
// budget (1/symbol/24h) remains the binding anti-saw guard. Any older
// non-flip protective close on the symbol keeps the cooldown armed.
func (worker *Worker) cascadeFlipCooldownExemptReal(ctx context.Context, accountID, symbol string) bool {
	if worker.db == nil {
		return false
	}
	var nonFlipCloses int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE account_id = $1 AND symbol = $2
		  AND status IN ('STOPPED', 'LIQUIDATED')
		  AND COALESCE(closed_reason, '') NOT IN (
		      `+protectiveCloseExemptReasons+`)
		  AND COALESCE(closed_reason, '') NOT IN (`+flipFamilyReasons+`)
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '24 hours'
	`, accountID, symbol).Scan(&nonFlipCloses); err != nil || nonFlipCloses > 0 {
		return false
	}
	return worker.flipEventWithin24h(ctx, symbol)
}

func (worker *Worker) cascadeFlipCooldownExemptPaper(ctx context.Context, settingsID, symbol string) bool {
	if worker.db == nil {
		return false
	}
	var nonFlipCloses int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots
		WHERE settings_id = $1 AND symbol = $2
		  AND status = 'COMPLETED'
		  AND COALESCE(closed_reason, '') NOT IN (
		      `+protectiveCloseExemptReasons+`)
		  AND COALESCE(closed_reason, '') NOT IN (`+flipFamilyReasons+`)
		  AND closed_at > NOW() - INTERVAL '24 hours'
	`, settingsID, symbol).Scan(&nonFlipCloses); err != nil || nonFlipCloses > 0 {
		return false
	}
	return worker.flipEventWithin24h(ctx, symbol)
}

// flipEventWithin24h (v2.0.142, audit P3-4) matches only REAL flip events:
// DIRECTION_FLIP is written solely by the cascade-short flip experiment's
// REAL handoff, but the query used to consult ANY source. Scoping to
// bot_source = 'REAL' is deliberately still fleet-wide for the PAPER arm —
// a REAL flip exempts the paper cooldown too, because paper exists to train
// the exact pipeline REAL rides (the same joint-fleet logic as the circuit
// breaker); the filter only stops a hypothetical non-REAL flip row from
// arming an exemption its fleet never earned.
func (worker *Worker) flipEventWithin24h(ctx context.Context, symbol string) bool {
	if worker.db == nil {
		return false
	}
	var flips int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE symbol = $1 AND event_type = 'DIRECTION_FLIP'
		  AND bot_source = 'REAL'
		  AND created_at > NOW() - INTERVAL '24 hours'
	`, symbol).Scan(&flips); err != nil {
		return false // fail-closed: no proof of a consumed flip — cooldown stands
	}
	return flips > 0
}
