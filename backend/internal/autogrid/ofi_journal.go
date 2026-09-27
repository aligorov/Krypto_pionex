package autogrid

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
)

// OFI decision journal (v2.0.138): the microstructure engine is memory-only,
// so every OFI-influenced decision point (entry veto, radar pass, adjust
// freeze, re-entry gate, emergency exit) writes its feature vector here.
// Without these rows the OFI-dependent decisions cannot be audited or
// replayed later — the package-VII data foundation.
//
// Write-only and failure-tolerant by contract: a telemetry failure must
// never change, delay or block the trading decision it observes.

// OFIDecision kinds (ofi_decision_snapshots.kind).
const (
	ofiKindEntryVeto    = "ENTRY_VETO"
	ofiKindRadar        = "RADAR"
	ofiKindAdjustFreeze = "ADJUST_FREEZE"
	ofiKindReentryGate  = "REENTRY_GATE"
	ofiKindEmergency    = "EMERGENCY"
)

// logOFIDecision persists one snapshot row. regime/readiness arrive as
// strings so callers decide how to derive them (Analyze().Regime string and
// the readiness contract), keeping this file independent of engine reshapes.
func logOFIDecision(ctx context.Context, db *pgxpool.Pool, settingsID, symbol, kind string,
	analysis marketdata.MicrostructureAnalysis, readiness, verdict, vetoReason, refID string) {
	if db == nil || symbol == "" {
		return
	}
	_, _ = db.Exec(ctx, `
		INSERT INTO ofi_decision_snapshots (
			symbol, kind, regime, readiness,
			ofi, taker_delta_usdt, micro_price_bias_bps, spread_bps,
			windows_stable, verdict, veto_reason, config_version, ref_id
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11, $12, $13
		)
	`,
		symbol, kind, string(analysis.Regime), readiness,
		roundNullable(analysis.CurrentOFI), roundNullable(analysis.TakerDeltaUSDT),
		roundNullable(analysis.MicroPriceBiasBps), roundNullable(analysis.CurrentSpreadBps),
		analysis.ConsecutiveBullWindows+analysis.ConsecutiveBearWindows,
		verdict, vetoReason,
		ConfigVersion(ctx, db, settingsID), refID,
	)
}

func roundNullable(v float64) *float64 {
	if v == 0 {
		return nil
	}
	r := float64(int(v*100+0.5)) / 100 // 2dp is plenty for a snapshot
	if v < 0 {
		r = float64(int(v*100-0.5)) / 100
	}
	return &r
}
