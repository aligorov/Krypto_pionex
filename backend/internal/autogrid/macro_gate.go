package autogrid

import (
	"context"
	"fmt"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
)

// Macro entry vetoes fed by CoinGecko snapshots (migration 0027).
//
// Two regimes kill NEUTRAL grids that pair-level indicators do not see:
//   - beta-drift: BTC itself trending down hard (24h ≤ −3%) — the whole
//     market carries one-way risk into a "range" that stops being one;
//   - alt-drain: BTC flat while its dominance climbs ≥ +0.35pp over 24h —
//     capital rotates out of alts precisely into the pairs this fleet
//     harvests (2026-08-30 night: BTC −0.5%..+1.2% flat, SPX −37%, eight
//     NEUTRAL bots died −$56.71 while all four shorts won).
//
// Both vetoes apply to non-short entries only; shorts and cascade mode are
// exempt. Until ~24h of snapshot history exists the gate records telemetry
// and passes (fail-open), mirroring how depthGate ramped up.

const (
	macroBetaDriftPct  = -3.0              // BTC 24h return at/below → veto
	macroAltDrainDelta = 0.35              // dominance delta (pp) over ≥20h → veto
	macroAltDrainFloor = -2.0              // ...only while BTC is flatter than this
	macroHistoryAge    = 20 * time.Hour    // minimum window before domDelta arms
)

type macroContext struct {
	btc24h   *float64
	domDelta *float64 // dominance percentage-point change over the window
	stale    bool     // latest snapshot older than 1h
	loaded   bool
}

// loadMacroContext reads the CoinGecko window once per deploy round.
func (worker *Worker) loadMacroContext(ctx context.Context) macroContext {
	latest, aged, err := marketdata.LatestCoinGeckoWindow(ctx, worker.db, macroHistoryAge)
	if err != nil || latest == nil {
		return macroContext{loaded: false}
	}
	mc := macroContext{loaded: true}
	btc := latest.BTC24hPct
	mc.btc24h = &btc
	if time.Since(latest.CapturedAt) > time.Hour {
		mc.stale = true
	}
	if aged != nil && latest.BTCDominancePct > 0 && aged.BTCDominancePct > 0 {
		d := latest.BTCDominancePct - aged.BTCDominancePct
		mc.domDelta = &d
	}
	return mc
}

// macroVeto decides for one candidate trend ("long" / "no_trend" / "short",
// the direction-engine output the scanner persisted). It never vetoes shorts
// or cascade entries, and never vetoes on missing/stale data.
func macroVeto(trend string, cascadeShort bool, mc macroContext) (bool, string, map[string]any) {
	telemetry := map[string]any{"macroGate": map[string]any{
		"btc24h":   derefFloat(mc.btc24h),
		"domDelta": derefFloat(mc.domDelta),
		"stale":    mc.stale,
		"armed":    mc.loaded && !mc.stale,
	}}
	if !mc.loaded || mc.stale || cascadeShort || trend == "short" {
		return false, "", telemetry
	}
	if mc.btc24h != nil && *mc.btc24h <= macroBetaDriftPct {
		return true,
			fmt.Sprintf("macro beta: BTC 24ч %.1f%% — тренд-день вниз, NEUTRAL/LONG отложены", *mc.btc24h),
			telemetry
	}
	if mc.domDelta != nil && *mc.domDelta >= macroAltDrainDelta &&
		(mc.btc24h == nil || *mc.btc24h > macroAltDrainFloor) {
		return true,
			fmt.Sprintf("macro alt-drain: доминация BTC +%.2fпп за 24ч при плоском BTC — альты понижаются, нейтральный сбор отложен", *mc.domDelta),
			telemetry
	}
	return false, "", telemetry
}

func derefFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
